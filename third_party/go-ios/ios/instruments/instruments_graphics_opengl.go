package instruments

import (
	"fmt"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/golog"
)

type graphicsOpenGLMsgDispatcher struct {
	messages chan dtx.Message
}

func newGraphicsOpenGLMsgDispatcher() *graphicsOpenGLMsgDispatcher {
	return &graphicsOpenGLMsgDispatcher{make(chan dtx.Message)}
}

func (p *graphicsOpenGLMsgDispatcher) Dispatch(m dtx.Message) {
	p.messages <- m
}

type GraphicsOpenGLService struct {
	channel       *dtx.Channel
	conn          *dtx.Connection
	msgDispatcher *graphicsOpenGLMsgDispatcher
}

type FramesPerSecondSample struct {
	CoreAnimationFramesPerSecond float64
}

func NewGraphicsOpenGLService(device ios.DeviceEntry) (*GraphicsOpenGLService, error) {
	msgDispatcher := newGraphicsOpenGLMsgDispatcher()
	dtxConn, err := connectInstrumentsWithMsgDispatcher(device, msgDispatcher)
	if err != nil {
		return nil, err
	}

	channel := dtxConn.RequestChannelIdentifier(graphicsOpenGLChannel, loggingDispatcher{dtxConn})
	if _, err := channel.MethodCall("startSamplingAtTimeInterval:", 0); err != nil {
		dtxConn.Close()
		return nil, err
	}

	return &GraphicsOpenGLService{channel: channel, conn: dtxConn, msgDispatcher: msgDispatcher}, nil
}

func (s *GraphicsOpenGLService) Close() error {
	close(s.msgDispatcher.messages)
	_ = s.channel.MethodCallAsync("stopSampling:")
	return s.conn.Close()
}

func (s *GraphicsOpenGLService) ReceiveFramesPerSecondSamples() chan FramesPerSecondSample {
	messages := make(chan FramesPerSecondSample)
	go func() {
		defer close(messages)

		for msg := range s.msgDispatcher.messages {
			sample, err := mapToFramesPerSecondSample(msg)
			if err != nil {
				golog.Debug("expected FPS sample from global channel, but received different message", "module", logModule, "message", msg, "error", err)
				continue
			}

			messages <- sample
		}
	}()

	return messages
}

func mapToFramesPerSecondSample(msg dtx.Message) (FramesPerSecondSample, error) {
	if len(msg.Payload) == 0 {
		return FramesPerSecondSample{}, fmt.Errorf("empty graphics OpenGL payload")
	}

	data, ok := msg.Payload[0].(map[string]interface{})
	if !ok {
		return FramesPerSecondSample{}, fmt.Errorf("graphics OpenGL payload should be map[string]interface{}: %T", msg.Payload[0])
	}

	value, ok := data["CoreAnimationFramesPerSecond"]
	if !ok {
		return FramesPerSecondSample{}, fmt.Errorf("missing CoreAnimationFramesPerSecond in graphics OpenGL payload")
	}

	fps, ok := toFloat64(value)
	if !ok {
		return FramesPerSecondSample{}, fmt.Errorf("CoreAnimationFramesPerSecond should be numeric: %T", value)
	}

	return FramesPerSecondSample{CoreAnimationFramesPerSecond: fps}, nil
}
