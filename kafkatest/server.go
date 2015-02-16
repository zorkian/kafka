package kafkatest

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"net"
	"strconv"
	"sync"

	"github.com/optiopay/kafka/proto"
)

const (
	AnyRequest              = -1
	ProduceRequest          = 0
	FetchRequest            = 1
	OffsetRequest           = 2
	MetadataRequest         = 3
	OffsetCommitRequest     = 8
	OffsetFetchRequest      = 9
	ConsumerMetadataRequest = 10
)

type Serializable interface {
	Bytes() ([]byte, error)
}

type RequestHandler func(request Serializable) (response Serializable)

func ComputeCrc(m *proto.Message) uint32 {
	var buf bytes.Buffer
	enc := proto.NewEncoder(&buf)
	enc.Encode(int8(0)) // magic byte is always 0
	enc.Encode(int8(0)) // no compression support
	enc.Encode(m.Key)
	enc.Encode(m.Value)
	return crc32.ChecksumIEEE(buf.Bytes())
}

type Server struct {
	Processed int

	mu       sync.RWMutex
	ln       net.Listener
	handlers map[int16]RequestHandler
}

func NewServer() *Server {
	srv := &Server{
		handlers: make(map[int16]RequestHandler),
	}
	srv.handlers[AnyRequest] = srv.defaultRequestHandler
	return srv
}

// Handle registers handler for given message kind. Handler registered with
// AnyRequest kind will be used only if there is no precise handler for the
// kind.
func (srv *Server) Handle(reqKind int16, handler RequestHandler) {
	srv.mu.Lock()
	srv.handlers[reqKind] = handler
	srv.mu.Unlock()
}

func (srv *Server) Address() string {
	return srv.ln.Addr().String()
}

func (srv *Server) HostPort() (string, int) {
	host, sport, err := net.SplitHostPort(srv.ln.Addr().String())
	if err != nil {
		panic(fmt.Sprintf("cannot split server address: %s", err))
	}
	port, err := strconv.Atoi(sport)
	if err != nil {
		panic(fmt.Sprintf("port '%s' is not a number: %s", sport, err))
	}
	if host == "" {
		host = "localhost"
	}
	return host, port
}

func (srv *Server) Start() {
	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.ln != nil {
		panic("server already started")
	}
	ln, err := net.Listen("tcp4", "")
	if err != nil {
		panic(fmt.Sprint("cannot start server: %s", err))
	}
	srv.ln = ln

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handleClient(client)
		}
	}()
}

func (srv *Server) Close() {
	srv.mu.Lock()
	srv.ln.Close()
	srv.mu.Unlock()
}

func (srv *Server) handleClient(c net.Conn) {
	for {
		kind, b, err := proto.ReadReq(c)
		if err != nil {
			return
		}
		srv.mu.RLock()
		fn, ok := srv.handlers[kind]
		if !ok {
			fn, ok = srv.handlers[AnyRequest]
		}
		srv.mu.RUnlock()

		if !ok {
			panic(fmt.Sprintf("no handler for %d", kind))
		}

		var request Serializable

		switch kind {
		case FetchRequest:
			request, err = proto.ReadFetchReq(bytes.NewBuffer(b))
		case ProduceRequest:
			request, err = proto.ReadProduceReq(bytes.NewBuffer(b))
		case OffsetRequest:
			request, err = proto.ReadOffsetReq(bytes.NewBuffer(b))
		case MetadataRequest:
			request, err = proto.ReadMetadataReq(bytes.NewBuffer(b))
		case ConsumerMetadataRequest:
			request, err = proto.ReadConsumerMetadataReq(bytes.NewBuffer(b))
		case OffsetCommitRequest:
			request, err = proto.ReadOffsetCommitReq(bytes.NewBuffer(b))
		case OffsetFetchRequest:
			request, err = proto.ReadOffsetFetchReq(bytes.NewBuffer(b))
		}

		if err != nil {
			panic(fmt.Sprintf("could not read message %d: %s", kind, err))
		}

		response := fn(request)
		if response != nil {
			b, err := response.Bytes()
			if err != nil {
				panic(fmt.Sprintf("cannot serialize %T: %s", response, err))
			}
			c.Write(b)
		}
	}
}

func (srv *Server) defaultRequestHandler(request Serializable) Serializable {
	srv.mu.RLock()
	defer srv.mu.RUnlock()

	switch req := request.(type) {
	case *proto.FetchReq:
		resp := &proto.FetchResp{
			CorrelationID: req.CorrelationID,
			Topics:        make([]proto.FetchRespTopic, len(req.Topics)),
		}
		for ti, topic := range req.Topics {
			resp.Topics[ti] = proto.FetchRespTopic{
				Name:       topic.Name,
				Partitions: make([]proto.FetchRespPartition, len(topic.Partitions)),
			}
			for pi, part := range topic.Partitions {
				resp.Topics[ti].Partitions[pi] = proto.FetchRespPartition{
					ID:        part.ID,
					Err:       proto.ErrUnknownTopicOrPartition,
					TipOffset: -1,
					Messages:  []*proto.Message{},
				}
			}
		}
		return resp
	case *proto.ProduceReq:
		resp := &proto.ProduceResp{
			CorrelationID: req.CorrelationID,
		}
		resp.Topics = make([]proto.ProduceRespTopic, len(req.Topics))
		for ti, topic := range req.Topics {
			resp.Topics[ti] = proto.ProduceRespTopic{
				Name:       topic.Name,
				Partitions: make([]proto.ProduceRespPartition, len(topic.Partitions)),
			}
			for pi, part := range topic.Partitions {
				resp.Topics[ti].Partitions[pi] = proto.ProduceRespPartition{
					ID:     part.ID,
					Err:    proto.ErrUnknownTopicOrPartition,
					Offset: -1,
				}
			}
		}
		return resp
	case *proto.OffsetReq:
		panic("not implemented")
		return &proto.OffsetResp{
			CorrelationID: req.CorrelationID,
		}
	case *proto.MetadataReq:
		host, sport, err := net.SplitHostPort(srv.ln.Addr().String())
		if err != nil {
			panic(fmt.Sprintf("cannot split server address: %s", err))
		}
		port, err := strconv.Atoi(sport)
		if err != nil {
			panic(fmt.Sprintf("port '%s' is not a number: %s", sport, err))
		}
		if host == "" {
			host = "localhost"
		}
		return &proto.MetadataResp{
			CorrelationID: req.CorrelationID,
			Brokers: []proto.MetadataRespBroker{
				proto.MetadataRespBroker{NodeID: 1, Host: host, Port: int32(port)},
			},
			Topics: []proto.MetadataRespTopic{},
		}
	case *proto.ConsumerMetadataReq:
		panic("not implemented")
		return &proto.ConsumerMetadataResp{
			CorrelationID: req.CorrelationID,
		}
	case *proto.OffsetCommitReq:
		panic("not implemented")
		return &proto.OffsetCommitResp{
			CorrelationID: req.CorrelationID,
		}
	case *proto.OffsetFetchReq:
		panic("not implemented")
		return &proto.OffsetFetchReq{
			CorrelationID: req.CorrelationID,
		}
	default:
		panic(fmt.Sprintf("unknown message type: %T", req))
	}
	return nil
}