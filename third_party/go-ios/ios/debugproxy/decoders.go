package debugproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/golog"
)

type decoder interface {
	decode([]byte)
}

type dtxDecoder struct {
	jsonFilePath string
	binFilePath  string
	buffer       bytes.Buffer
	isBroken     bool
	log          *slog.Logger
}

type MessageWithMetaInfo struct {
	DtxMessage   interface{}
	MessageType  string
	TimeReceived time.Time
	OffsetInDump int64
	Length       int
}

func NewDtxDecoder(jsonFilePath string, binFilePath string, log *slog.Logger) decoder {
	return &dtxDecoder{jsonFilePath: jsonFilePath, binFilePath: binFilePath, buffer: bytes.Buffer{}, isBroken: false, log: log}
}

func (f *dtxDecoder) decode(data []byte) {
	file, err := os.OpenFile(f.binFilePath+".raw",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		golog.Info("failed opening raw dump file", "module", logModule, "binFilePath", f.binFilePath, "error", err)
	}

	file.Write(data)
	file.Close()

	if f.isBroken {
		// when an error happens while decoding, this flag prevents from flooding the logs with errors
		// while still dumping binary to debug later
		return
	}
	f.buffer.Write(data)
	slice := f.buffer.Next(f.buffer.Len())
	written := 0
	for {
		msg, remainingbytes, err := dtx.DecodeNonBlocking(slice)
		if dtx.IsIncomplete(err) {
			f.buffer.Reset()
			f.buffer.Write(slice)
			break
		}
		if err != nil {
			f.log.Error("failed decoding DTX, continuing bindumping", "error", err, "bytes", fmt.Sprintf("%x", slice))
			f.isBroken = true
		}
		slice = remainingbytes

		file, err := os.OpenFile(f.binFilePath,
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			golog.Info("failed opening bin file", "module", logModule, "binFilePath", f.binFilePath, "error", err)
		}
		s, _ := file.Stat()
		offset := s.Size()
		file.Write(msg.RawBytes)
		file.Close()

		file, err = os.OpenFile(f.jsonFilePath,
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			golog.Info("failed opening json file", "module", logModule, "jsonFilePath", f.jsonFilePath, "error", err)
		}

		type Alias dtx.Message
		auxi := ""
		if msg.HasAuxiliary() {
			auxi = msg.Auxiliary.String()
		}
		aux := &struct {
			AuxiliaryContents string
			*Alias
		}{
			AuxiliaryContents: auxi,
			Alias:             (*Alias)(&msg),
		}
		aux.RawBytes = nil
		jsonMetaInfo := MessageWithMetaInfo{aux, "dtx", time.Now(), offset, len(msg.RawBytes)}

		mylog := f.log
		if strings.Contains(f.binFilePath, "from-device") {
			mylog = f.log.With("d", "in")
		}
		if strings.Contains(f.binFilePath, "to-device") {
			mylog = f.log.With("d", "out")
		}
		logDtxMessageNice(mylog, msg)
		jsonmsg, err := json.Marshal(jsonMetaInfo)
		file.Write(jsonmsg)
		io.WriteString(file, "\n")
		file.Close()

		written += len(msg.RawBytes)
	}
}

func logDtxMessageNice(log *slog.Logger, msg dtx.Message) {
	if msg.PayloadHeader.MessageType == dtx.Methodinvocation {
		expectsReply := ""
		if msg.ExpectsReply {
			expectsReply = "e"
		}
		log.Info("dtx method invocation",
			"identifier", msg.Identifier, "conversationIndex", msg.ConversationIndex,
			"expectsReply", expectsReply, "channelCode", msg.ChannelCode,
			"method", msg.Payload[0], "auxiliary", msg.Auxiliary)
		return
	}
	if msg.PayloadHeader.MessageType == dtx.Ack {
		log.Info("dtx ack",
			"identifier", msg.Identifier, "conversationIndex", msg.ConversationIndex, "channelCode", msg.ChannelCode)
		return
	}
	if msg.PayloadHeader.MessageType == dtx.UnknownTypeOne {
		if len(msg.Payload) > 0 {
			log.Info("dtx type1 with payload", "payload", fmt.Sprintf("%x", msg.Payload[0]))
			return
		}
		log.Info("dtx type1 without payload", "message", fmt.Sprintf("%+v", msg))
		return
	}
	if msg.PayloadHeader.MessageType == dtx.ResponseWithReturnValueInPayload {
		log.Info("dtx response",
			"identifier", msg.Identifier, "conversationIndex", msg.ConversationIndex,
			"channelCode", msg.ChannelCode, "response", msg.Payload[0])
		return
	}
	if msg.PayloadHeader.MessageType == dtx.DtxTypeError {
		log.Info("dtx error",
			"identifier", msg.Identifier, "conversationIndex", msg.ConversationIndex,
			"channelCode", msg.ChannelCode, "error", msg.Payload[0])
		return
	}
	log.Info("dtx message", "message", fmt.Sprintf("%+v", msg))
}

type binaryOnlyDumper struct {
	path string
}

// NewNoOpDecoder does nothing
func NewBinDumpOnly(jsonFilePath string, dumpFilePath string, log *slog.Logger) decoder {
	return binaryOnlyDumper{dumpFilePath}
}

func (n binaryOnlyDumper) decode(bytes []byte) {
	writeBytes(n.path, bytes)
}

func writeBytes(filePath string, data []byte) {
	file, err := os.OpenFile(filePath,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(fmt.Sprintf("Could not write to file error: %v path:'%s'", err, filePath))
	}

	file.Write(data)
	file.Close()
}
