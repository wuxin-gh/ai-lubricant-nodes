package debugproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"

	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
	"howett.net/plist"
)

func proxyUsbMuxConnection(p *ProxyConnection, muxOnUnixSocket *ios.UsbMuxConnection, muxToDevice *ios.UsbMuxConnection) {
	defer func() {
		golog.Info("done", "module", logModule, "id", p.id) // logged normally even if there is a panic
		if x := recover(); x != nil {
			golog.Info("run time panic, moving back socket", "module", logModule, "id", p.id, "panic", x)
			err := MoveBack(ios.ToUnixSocketPath(ios.GetUsbmuxdSocket()))
			if err != nil {
				golog.Error("failed moving back socket", "module", logModule, "id", p.id, "error", err)
			}
			panic(x)
		}
	}()
	for {
		request, err := muxOnUnixSocket.ReadMessage()
		if err != nil {
			muxOnUnixSocket.ReleaseDeviceConnection().Close()
			muxToDevice.ReleaseDeviceConnection().Close()
			if err == io.EOF {
				p.LogClosed()
				return
			}
			p.log.Info("failed reading usbmux message", "error", err)
			return
		}

		var decodedRequest map[string]interface{}
		decoder := plist.NewDecoder(bytes.NewReader(request.Payload))
		err = decoder.Decode(&decodedRequest)
		if err != nil {
			p.log.Info("failed decoding mux message", "request", request, "error", err)
		}
		p.logJSONMessageToDevice(map[string]interface{}{"header": request.Header, "payload": decodedRequest, "type": "USBMUX"})

		p.log.With("ID", p.id, "direction", "host->device").Log(context.Background(), golog.LevelTrace, "usbmux request", "request", decodedRequest)
		if decodedRequest["MessageType"] == "Connect" {
			handleConnect(request, decodedRequest, p, muxOnUnixSocket, muxToDevice)
			return
		}

		err = muxToDevice.SendMuxMessage(request)

		if decodedRequest["MessageType"] == "ReadPairRecord" {
			handleReadPairRecord(p, muxOnUnixSocket, muxToDevice)
			continue
		}
		if err != nil {
			panic(fmt.Sprintf("Failed forwarding message to device: %+v", request))
		}
		if decodedRequest["MessageType"] == "Listen" {
			handleListen(p, muxOnUnixSocket, muxToDevice)
			return
		}

		response, err := muxToDevice.ReadMessage()
		if err != nil {
			p.log.Error("failed muxToDevice.ReadMessage()", "request", request, "error", err)
		}
		var decodedResponse map[string]interface{}
		decoder = plist.NewDecoder(bytes.NewReader(response.Payload))
		err = decoder.Decode(&decodedResponse)
		if err != nil {
			p.log.Error("failed decoding mux message", "response", decodedResponse, "error", err)
		}
		p.logJSONMessageFromDevice(map[string]interface{}{"header": response.Header, "payload": decodedResponse, "type": "USBMUX"})
		p.log.With("ID", p.id, "direction", "device->host").Log(context.Background(), golog.LevelTrace, "usbmux response", "response", decodedResponse)
		err = muxOnUnixSocket.SendMuxMessage(response)
		if err != nil {
			p.log.Error("failed muxOnUnixSocket.SendMuxMessage(response)", "request", request, "error", err)
		}
	}
}

func handleReadPairRecord(p *ProxyConnection, muxOnUnixSocket *ios.UsbMuxConnection, muxToDevice *ios.UsbMuxConnection) {
	response, err := muxToDevice.ReadMessage()
	var decodedResponse map[string]interface{}
	decoder := plist.NewDecoder(bytes.NewReader(response.Payload))
	err = decoder.Decode(&decodedResponse)
	if err != nil {
		p.log.Info("failed decoding mux message", "response", decodedResponse, "error", err)
	}
	pairRecord := ios.PairRecordfromBytes(decodedResponse["PairRecordData"].([]byte))
	pairRecord.DeviceCertificate = pairRecord.HostCertificate
	decodedResponse["PairRecordData"] = []byte(ios.ToPlist(pairRecord))
	newPayload := []byte(ios.ToPlist(decodedResponse))
	response.Payload = newPayload
	response.Header.Length = uint32(len(newPayload) + 16)
	p.logJSONMessageFromDevice(map[string]interface{}{"header": response.Header, "payload": decodedResponse, "type": "USBMUX"})
	p.log.With("ID", p.id, "direction", "device->host").Log(context.Background(), golog.LevelTrace, "usbmux response", "response", decodedResponse)
	err = muxOnUnixSocket.SendMuxMessage(response)
}

func handleConnect(connectRequest ios.UsbMuxMessage, decodedConnectRequest map[string]interface{}, p *ProxyConnection, muxOnUnixSocket *ios.UsbMuxConnection, muxToDevice *ios.UsbMuxConnection) {
	var port uint16
	portFromPlist := decodedConnectRequest["PortNumber"]
	switch portFromPlist.(type) {
	case uint64:
		port = uint16(portFromPlist.(uint64))

	case int64:
		port = uint16(portFromPlist.(int64))
	}

	if port == ios.Lockdownport {
		p.log.Log(context.Background(), golog.LevelTrace, "connect to lockdown")
		handleConnectToLockdown(connectRequest, decodedConnectRequest, p, muxOnUnixSocket, muxToDevice)
	} else {
		info, err := p.debugProxy.retrieveServiceInfoByPort(ios.Ntohs(uint16(port)))
		if err != nil {
			panic(fmt.Sprintf("ServiceInfo for port: %d not found, this is a bug :-)reqheader: %+v repayload: %x", port, connectRequest.Header, connectRequest.Payload))
		}
		p.log.Info("connection to service detected", "service", info.ServiceName, "port", info.ServicePort)
		handleConnectToService(connectRequest, decodedConnectRequest, p, muxOnUnixSocket, muxToDevice, info)
	}
}

func handleConnectToLockdown(connectRequest ios.UsbMuxMessage, decodedConnectRequest map[string]interface{}, p *ProxyConnection, muxOnUnixSocket *ios.UsbMuxConnection, muxToDevice *ios.UsbMuxConnection) {
	err := muxToDevice.SendMuxMessage(connectRequest)
	if err != nil {
		panic("Failed sending muxmessage to device")
	}
	connectResponse, err := muxToDevice.ReadMessage()
	muxOnUnixSocket.SendMuxMessage(connectResponse)

	lockdownToDevice := ios.NewLockDownConnection(muxToDevice.ReleaseDeviceConnection())
	lockdownOnUnixSocket := ios.NewLockDownConnection(muxOnUnixSocket.ReleaseDeviceConnection())
	proxyLockDownConnection(p, lockdownOnUnixSocket, lockdownToDevice)
}

func handleListen(p *ProxyConnection, muxOnUnixSocket *ios.UsbMuxConnection, muxToDevice *ios.UsbMuxConnection) {
	go func() {
		// use this to detect when the conn is closed. There shouldn't be any messages received ever.
		_, err := muxOnUnixSocket.ReadMessage()
		if err == io.EOF {
			muxOnUnixSocket.ReleaseDeviceConnection().Close()
			muxToDevice.ReleaseDeviceConnection().Close()
			p.LogClosed()
			return
		}
		p.log.Error("unexpected error on read for LISTEN connection", "error", err)
	}()

	for {
		response, err := muxToDevice.ReadMessage()
		if err != nil {
			// TODO: ugly, improve
			d := muxOnUnixSocket.ReleaseDeviceConnection()
			d1 := muxToDevice.ReleaseDeviceConnection()
			if d != nil {
				d.Close()
			}
			if d1 != nil {
				d1.Close()
			}

			p.LogClosed()
			return
		}
		var decodedResponse map[string]interface{}
		decoder := plist.NewDecoder(bytes.NewReader(response.Payload))
		err = decoder.Decode(&decodedResponse)
		if err != nil {
			p.log.Info("failed decoding mux message", "response", decodedResponse, "error", err)
		}
		p.logJSONMessageFromDevice(map[string]interface{}{"header": response.Header, "payload": decodedResponse, "type": "USBMUX"})
		p.log.With("ID", p.id, "direction", "device->host").Log(context.Background(), golog.LevelTrace, "usbmux response", "response", decodedResponse)
		err = muxOnUnixSocket.SendMuxMessage(response)
		if err != nil {
			p.log.Info("failed muxOnUnixSocket.SendMuxMessage(response)", "response", decodedResponse, "error", err)
		}
	}
}
