// Package desktop owns the shared portal session used by media and input.
package desktop

import (
	"context"
	"errors"
	"fmt"

	"github.com/tarik02/webdesktop/capture"
	"github.com/tarik02/webdesktop/input"
	"github.com/tarik02/webdesktop/input/eis"
	"go.uber.org/zap"
)

// Media runs the shared video pipeline against one authorized portal session.
type Media interface {
	Run(context.Context, *capture.Session) error
}

// Service owns the portal session and coordinates its media and input users.
type Service struct {
	cfg    capture.Config
	media  Media
	input  *input.Controller
	logger *zap.Logger
}

// New constructs the shared desktop service.
func New(
	cfg capture.Config,
	mediaService Media,
	inputController *input.Controller,
	logger *zap.Logger,
) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if mediaService == nil {
		return nil, errors.New("desktop media service is required")
	}
	if inputController == nil {
		return nil, errors.New("desktop input controller is required")
	}
	if logger == nil {
		return nil, errors.New("desktop logger is required")
	}
	return &Service{
		cfg:    cfg,
		media:  mediaService,
		input:  inputController,
		logger: logger,
	}, nil
}

// Run authorizes one portal session and keeps it alive for media and input.
func (s *Service) Run(ctx context.Context) (runErr error) {
	session, err := capture.Open(ctx, s.cfg)
	if err != nil {
		return fmt.Errorf("open desktop portal session: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, session.Close())
	}()
	defer func() {
		runErr = errors.Join(runErr, s.input.Close())
	}()

	authorization := session.InputAuthorization()
	if authorization.Enabled {
		backend, err := session.AcquireEIS()
		if err != nil {
			return fmt.Errorf("acquire portal EIS connection: %w", err)
		}
		sender, err := eis.New(backend, eis.Config{
			Name:      "webdesktop",
			Pointer:   s.cfg.Input.Pointer,
			Keyboard:  s.cfg.Input.Keyboard,
			MappingID: session.MappingID(),
		})
		if err != nil {
			return fmt.Errorf("start EIS sender: %w", err)
		}
		if err := s.input.Attach(input.Authorization{
			Pointer:  authorization.Pointer,
			Keyboard: authorization.Keyboard,
		}, sender); err != nil {
			return fmt.Errorf("attach EIS sender: %w", err)
		}

		mediaCtx, cancelMedia := context.WithCancel(ctx)
		defer cancelMedia()
		mediaResult := make(chan error, 1)
		go func() {
			mediaResult <- s.media.Run(mediaCtx, session)
		}()

		select {
		case mediaErr := <-mediaResult:
			select {
			case <-sender.Done():
				status := sender.Status()
				if status.Err == nil {
					status.Err = errors.New("EIS sender stopped")
				}
				return errors.Join(mediaErr, fmt.Errorf("EIS input stopped: %w", status.Err))
			default:
				return mediaErr
			}
		case <-sender.Done():
			status := sender.Status()
			if status.Err == nil {
				status.Err = errors.New("EIS sender stopped")
			}
			cancelMedia()
			mediaErr := <-mediaResult
			return errors.Join(fmt.Errorf("EIS input stopped: %w", status.Err), mediaErr)
		}
	}

	return s.media.Run(ctx, session)
}
