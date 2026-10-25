package instruments

import (
	"fmt"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/golog"
)

type networkMsgDispatcher struct {
	messages chan dtx.Message
}

func newNetworkMsgDispatcher() *networkMsgDispatcher {
	return &networkMsgDispatcher{make(chan dtx.Message)}
}

func (p *networkMsgDispatcher) Dispatch(m dtx.Message) {
	p.messages <- m
}

type NetworkService struct {
	channel       *dtx.Channel
	conn          *dtx.Connection
	msgDispatcher *networkMsgDispatcher
}

type NetworkSample struct {
	Type uint64
	Data map[string]interface{}
}

func NewNetworkService(device ios.DeviceEntry) (*NetworkService, error) {
	msgDispatcher := newNetworkMsgDispatcher()
	dtxConn, err := connectInstrumentsWithMsgDispatcher(device, msgDispatcher)
	if err != nil {
		return nil, err
	}

	channel := dtxConn.RequestChannelIdentifier(mobileNetworkingChannel, loggingDispatcher{dtxConn})
	if _, err := channel.MethodCall("startMonitoring"); err != nil {
		dtxConn.Close()
		return nil, err
	}

	return &NetworkService{channel: channel, conn: dtxConn, msgDispatcher: msgDispatcher}, nil
}

func (s *NetworkService) Close() error {
	close(s.msgDispatcher.messages)
	_ = s.channel.MethodCallAsync("stopMonitoring")
	return s.conn.Close()
}

func (s *NetworkService) ReceiveNetworkSamples() chan NetworkSample {
	messages := make(chan NetworkSample)
	go func() {
		defer close(messages)

		for msg := range s.msgDispatcher.messages {
			sample, err := mapToNetworkSample(msg)
			if err != nil {
				golog.Debug("expected network sample from global channel, but received different message", "module", logModule, "message", msg, "error", err)
				continue
			}

			messages <- sample
		}
	}()

	return messages
}

func mapToNetworkSample(msg dtx.Message) (NetworkSample, error) {
	if len(msg.Payload) < 2 {
		return NetworkSample{}, fmt.Errorf("network payload should have at least two elements: %+v", msg.Payload)
	}

	sampleType, ok := toUint64(msg.Payload[0])
	if !ok {
		return NetworkSample{}, fmt.Errorf("network sample type should be numeric: %T", msg.Payload[0])
	}

	data, ok := msg.Payload[1].(map[string]interface{})
	if !ok {
		return NetworkSample{}, fmt.Errorf("network sample data should be map[string]interface{}: %T", msg.Payload[1])
	}

	return NetworkSample{Type: sampleType, Data: data}, nil
}
