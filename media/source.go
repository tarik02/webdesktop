package media

import (
	"errors"

	"github.com/tarik02/webdesktop/capture"
)

// Source opens one PipeWire stream for the video pipeline.
type Source interface {
	AcquireStream() (*SourceStream, error)
	Done() <-chan struct{}
	Err() error
}

// SourceStream identifies one PipeWire stream and owns its connection.
type SourceStream struct {
	PipeWireFD        int
	NodeID            uint32
	PipeWireSerial    uint64
	HasPipeWireSerial bool
	TargetObject      string
	Release           func() error
}

// Close releases the stream connection.
func (stream *SourceStream) Close() error {
	if stream.Release == nil {
		return nil
	}
	return stream.Release()
}

type portalSource struct {
	session *capture.Session
}

// PortalSource adapts an authorized desktop portal session.
func PortalSource(session *capture.Session) Source {
	return portalSource{session: session}
}

func (source portalSource) AcquireStream() (*SourceStream, error) {
	if source.session == nil {
		return nil, errors.New("portal capture session is required")
	}
	stream, err := source.session.AcquireStream()
	if err != nil {
		return nil, err
	}
	return &SourceStream{
		PipeWireFD:        stream.PipeWireFD,
		NodeID:            stream.NodeID,
		PipeWireSerial:    stream.PipeWireSerial,
		HasPipeWireSerial: stream.HasPipeWireSerial,
		Release:           stream.Close,
	}, nil
}

func (source portalSource) Done() <-chan struct{} {
	return source.session.Done()
}

func (source portalSource) Err() error {
	return source.session.Err()
}

type pipeWireTargetSource struct {
	target string
}

// NewPipeWireTargetSource connects through the process PipeWire remote.
func NewPipeWireTargetSource(target string) (Source, error) {
	if target == "" {
		return nil, errors.New("PipeWire target object is required")
	}
	return pipeWireTargetSource{target: target}, nil
}

func (source pipeWireTargetSource) AcquireStream() (*SourceStream, error) {
	return &SourceStream{PipeWireFD: -1, TargetObject: source.target}, nil
}

func (pipeWireTargetSource) Done() <-chan struct{} {
	return nil
}

func (pipeWireTargetSource) Err() error {
	return nil
}
