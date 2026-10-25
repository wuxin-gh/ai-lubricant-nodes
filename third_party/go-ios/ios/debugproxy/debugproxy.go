package debugproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
)

const logModule = "go-ios/debugproxy"

const connectionJSONFileName = "connections.json"

// DebugProxy can be used to dump and modify communication between mac and host
type DebugProxy struct {
	mux               sync.Mutex
	serviceList       []PhoneServiceInformation
	connectionCounter int
	WorkingDir        string
}

// PhoneServiceInformation contains info about a service started on the phone via lockdown.
type PhoneServiceInformation struct {
	ServicePort uint16
	ServiceName string
	UseSSL      bool
}

// ProxyConnection keeps track of the pairRecord and uses an ID to identify connections.
type ProxyConnection struct {
	id         string
	pairRecord ios.PairRecord
	debugProxy *DebugProxy
	info       ConnectionInfo
	log        *slog.Logger
	mux        sync.Mutex
	closed     bool
}

type ConnectionInfo struct {
	ConnectionPath string
	CreatedAt      time.Time
	ID             string
}

func (p *ProxyConnection) LogClosed() {
	p.mux.Lock()
	defer p.mux.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.log.Log(context.Background(), golog.LevelTrace, "connection closed")
}

func (d *DebugProxy) storeServiceInformation(serviceInfo PhoneServiceInformation) {
	d.mux.Lock()
	defer d.mux.Unlock()
	d.serviceList = append(d.serviceList, serviceInfo)
}

func (d *DebugProxy) retrieveServiceInfoByPort(port uint16) (PhoneServiceInformation, error) {
	d.mux.Lock()
	defer d.mux.Unlock()
	for _, element := range d.serviceList {
		if element.ServicePort == port {
			return element, nil
		}
	}
	return PhoneServiceInformation{}, fmt.Errorf("No Service found for port %d", port)
}

// NewDebugProxy creates a new Default proxy
func NewDebugProxy() *DebugProxy {
	return &DebugProxy{mux: sync.Mutex{}, serviceList: []PhoneServiceInformation{}}
}

// Launch moves the original /var/run/usbmuxd to /var/run/usbmuxd.real and starts the server at /var/run/usbmuxd
func (d *DebugProxy) Launch(device ios.DeviceEntry, binaryMode bool) error {
	list, _ := ios.ListDevices()
	if len(list.DeviceList) > 1 {
		return fmt.Errorf("dproxy currently does not work when more than one device is connected to the host. please disconnect all but one device.")
	}
	if binaryMode {
		golog.Info("launching proxy in full binary mode", "module", logModule, "udid", device.Properties.SerialNumber)
	}
	var pairRecord ios.PairRecord
	if !binaryMode {
		var err error
		pairRecord, err = ios.ReadPairRecord(device.Properties.SerialNumber)
		if err != nil {
			return err
		}
		golog.Info("successfully retrieved pairrecord", "module", logModule, "udid", device.Properties.SerialNumber, "hostID", pairRecord.HostID)
	}
	originalSocket, err := MoveSock(ios.ToUnixSocketPath(ios.GetUsbmuxdSocket()))
	if err != nil {
		golog.Error("unable to move, lacking permissions?", "module", logModule, "udid", device.Properties.SerialNumber, "error", err, "socket", ios.GetUsbmuxdSocket())
		return err
	}
	d.setupDirectory()
	listener, err := net.Listen("unix", ios.ToUnixSocketPath(ios.GetUsbmuxdSocket()))
	if err != nil {
		golog.Error("could not listen on usbmuxd socket, do I have access permissions?", "module", logModule, "udid", device.Properties.SerialNumber, "error", err)
		return err
	}
	if err := os.Chmod(ios.ToUnixSocketPath(ios.GetUsbmuxdSocket()), 0o777); err != nil {
		golog.Error("could not change permission on usbmuxd socket", "module", logModule, "udid", device.Properties.SerialNumber, "error", err)
		return err
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			golog.Error("error with connection", "module", logModule, "udid", device.Properties.SerialNumber, "error", err)
		}
		golog.Info("connected", "module", logModule, "udid", device.Properties.SerialNumber)
		d.connectionCounter++
		id := fmt.Sprintf("#%d", d.connectionCounter)
		connectionPath := filepath.Join(".", d.WorkingDir, "connection-"+id+"-"+time.Now().UTC().Format("2006.01.02-15.04.05.000"))

		err = os.MkdirAll(connectionPath, os.ModePerm)
		if err != nil {
			golog.Error("failed mkdirall in connected", "module", logModule, "udid", device.Properties.SerialNumber, "id", id, "error", err)
		}

		info := ConnectionInfo{ConnectionPath: connectionPath, CreatedAt: time.Now(), ID: id}
		d.addConnectionInfoToJsonFile(info)

		bindumpHostProxyFile := filepath.Join(connectionPath, "bindump-hostservice-to-proxy.txt")

		if !binaryMode {
			// if the proxy is in full binary mode, there is no point in creating another binary dump
			golog.Info("creating binary dump of all communication between MAC OS and debugproxy", "module", logModule, "udid", device.Properties.SerialNumber, "id", id, "path", bindumpHostProxyFile)
			conn = NewDumpingConn(bindumpHostProxyFile, conn)
		}

		startProxyConnection(conn, originalSocket, pairRecord, d, info, binaryMode)
	}
}

func startProxyConnection(conn net.Conn, originalSocket string, pairRecord ios.PairRecord, debugProxy *DebugProxy, info ConnectionInfo, binaryMode bool) {
	golog.Info("starting tunnel", "module", logModule, "id", info.ID)
	devConn, err := ios.NewDeviceConnection(originalSocket)
	if err != nil {
		golog.Error("failed creating device connection", "module", logModule, "id", info.ID, "error", err)
		return
	}

	logger := golog.L().With("id", info.ID)
	p := ProxyConnection{info.ID, pairRecord, debugProxy, info, logger, sync.Mutex{}, false}

	if binaryMode {
		binOnUnixSocket := BinaryForwardingProxy{ios.NewDeviceConnectionWithConn(conn), NewBinDumpOnly("does not matter", filepath.Join(info.ConnectionPath, "rawbindump-from-host-service.bin"), logger)}
		binToDevice := BinaryForwardingProxy{devConn, NewBinDumpOnly("does not matter", filepath.Join(info.ConnectionPath, "rawbindump-from-device.bin"), logger)}
		go proxyBinDumpConnection(&p, binOnUnixSocket, binToDevice)
		return
	}
	connListeningOnUnixSocket := ios.NewUsbMuxConnection(ios.NewDeviceConnectionWithConn(conn))
	connectionToDevice := ios.NewUsbMuxConnection(devConn)
	go proxyUsbMuxConnection(&p, connListeningOnUnixSocket, connectionToDevice)
}

// Close moves /var/run/usbmuxd.real back to /var/run/usbmuxd and disconnects all active proxy connections
func (d *DebugProxy) Close() {
	golog.Info("moving back original socket", "module", logModule)
	err := MoveBack(ios.ToUnixSocketPath(ios.GetUsbmuxdSocket()))
	if err != nil {
		golog.Error("failed moving back socket", "module", logModule, "error", err)
	}
}

func (d *DebugProxy) setupDirectory() {
	newpath := filepath.Join(".", "dump-"+time.Now().UTC().Format("2006.01.02-15.04.05.000"))
	d.WorkingDir = newpath
	os.MkdirAll(newpath, os.ModePerm)
}

func (d *DebugProxy) addConnectionInfoToJsonFile(connInfo ConnectionInfo) {
	file, err := os.OpenFile(filepath.Join(d.WorkingDir, connectionJSONFileName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		golog.Info("failed opening connections json file", "module", logModule, "id", connInfo.ID, "error", err)
	}
	data, err := json.Marshal(connInfo)
	if err != nil {
		golog.Info("failed json", "module", logModule, "id", connInfo.ID, "error", err)
	}
	file.Write(data)
	io.WriteString(file, "\n")
	file.Close()
}

func (p *ProxyConnection) logJSONMessageFromDevice(msg map[string]interface{}) {
	const outPath = "jsondump.json"
	msg["direction"] = "device->host"
	writeJSON(filepath.Join(p.info.ConnectionPath, outPath), msg)
}

func (p *ProxyConnection) logJSONMessageToDevice(msg map[string]interface{}) {
	const outPath = "jsondump.json"
	msg["direction"] = "host->device"
	writeJSON(filepath.Join(p.info.ConnectionPath, outPath), msg)
}

func writeJSON(filePath string, JSON interface{}) {
	file, err := os.OpenFile(filePath,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(fmt.Sprintf("Could not write to file err: %v filepath:'%s'", err, filePath))
	}
	jsonmsg, err := json.Marshal(JSON)
	if err != nil {
		golog.Warn("error encoding to json", "module", logModule, "filePath", filePath, "value", JSON, "error", err)
	}
	file.Write(jsonmsg)
	io.WriteString(file, "\n")
	file.Close()
}
