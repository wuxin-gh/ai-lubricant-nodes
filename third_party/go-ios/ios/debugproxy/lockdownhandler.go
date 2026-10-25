package debugproxy

import (
	"bytes"
	"io"

	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
	"howett.net/plist"
)

func proxyLockDownConnection(p *ProxyConnection, lockdownOnUnixSocket *ios.LockDownConnection, lockdownToDevice *ios.LockDownConnection) {
	for {
		request, err := lockdownOnUnixSocket.ReadMessage()
		if err != nil {
			lockdownOnUnixSocket.Close()
			lockdownToDevice.Close()
			if err == io.EOF {
				p.LogClosed()
				return
			}
			p.log.Info("failed reading lockdown message", "error", err)
			return
		}

		var decodedRequest map[string]interface{}
		decoder := plist.NewDecoder(bytes.NewReader(request))
		err = decoder.Decode(&decodedRequest)
		if err != nil {
			p.log.Info("failed decoding lockdown message", "request", request, "error", err)
		}
		p.logJSONMessageToDevice(map[string]interface{}{"payload": decodedRequest, "type": "LOCKDOWN"})
		p.log.With("ID", p.id, "direction", "host2device").Info("lockdown request", "request", decodedRequest)

		err = lockdownToDevice.Send(decodedRequest)
		if err != nil {
			p.log.Error("failed forwarding message to device", "request", request)
		}
		p.log.Info("done sending to device")
		response, err := lockdownToDevice.ReadMessage()
		if err != nil {
			golog.Error("error reading from device", "module", logModule, "id", p.id, "error", err)
			response, err = lockdownToDevice.ReadMessage()
			golog.Info("second read", "module", logModule, "id", p.id, "response", response, "error", err)
		}

		var decodedResponse map[string]interface{}
		decoder = plist.NewDecoder(bytes.NewReader(response))
		err = decoder.Decode(&decodedResponse)
		if err != nil {
			p.log.Info("failed decoding lockdown message", "response", decodedResponse, "error", err)
		}
		p.logJSONMessageFromDevice(map[string]interface{}{"payload": decodedResponse, "type": "LOCKDOWN"})
		p.log.With("ID", p.id, "direction", "device2host").Info("lockdown response", "response", decodedResponse)

		err = lockdownOnUnixSocket.Send(decodedResponse)
		if err != nil {
			p.log.Info("failed sending lockdown message from device to host service", "response", decodedResponse, "error", err)
		}
		if decodedResponse["EnableSessionSSL"] == true {
			lockdownToDevice.EnableSessionSsl(p.pairRecord)
			lockdownOnUnixSocket.EnableSessionSslServerMode(p.pairRecord)
		}
		if decodedResponse["Request"] == "StartService" && decodedResponse["Error"] == nil {

			useSSL := false
			if decodedResponse["EnableServiceSSL"] != nil {
				useSSL = decodedResponse["EnableServiceSSL"].(bool)
			}
			info := PhoneServiceInformation{
				ServicePort: uint16(decodedResponse["Port"].(uint64)),
				ServiceName: decodedResponse["Service"].(string),
				UseSSL:      useSSL,
			}

			p.log.Debug("detected service start", "service", info)
			p.debugProxy.storeServiceInformation(info)

		}

		if decodedResponse["Request"] == "StopSession" {
			p.log.Info("stop session detected, disabling SSL")
			lockdownOnUnixSocket.DisableSessionSSL()
			lockdownToDevice.DisableSessionSSL()
		}
	}
}
