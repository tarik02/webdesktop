package webrtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	pion "github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

func (p *peer) onDataChannel(channel *pion.DataChannel) {
	if p.isClosing() {
		_ = channel.Close()
		return
	}
	switch channel.Label() {
	case "control":
		p.setControlChannel(channel)
	case "input":
		p.setInputChannel(channel)
	case "input-motion":
		p.setInputMotionChannel(channel)
	case "clipboard":
		if p.service.clipboard.Enabled() {
			p.setClipboardChannel(channel)
		} else {
			_ = channel.Close()
		}
	default:
		p.logger.Info("rejecting unsupported data channel", zap.String("label", channel.Label()))
		_ = channel.Close()
	}
}

func (p *peer) setControlChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting control data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.controlMu.Lock()
	previous := p.control
	p.control = channel
	p.controlMu.Unlock()
	if previous != nil {
		p.inputMu.Lock()
		p.inputLeaseGeneration++
		p.inputMu.Unlock()
		_ = p.service.input.Release(p.id)
		_ = previous.Close()
	}

	channel.OnOpen(func() {
		p.logger.Info("control data channel opened")
	})
	cleanup := func() {
		p.controlMu.Lock()
		owned := p.control == channel
		if owned {
			p.control = nil
		}
		p.controlMu.Unlock()
		if owned {
			p.inputMu.Lock()
			p.inputLeaseGeneration++
			p.inputMu.Unlock()
			_ = p.service.input.Release(p.id)
		}
	}
	channel.OnClose(func() { cleanup() })
	channel.OnError(func(err error) { p.logger.Debug("control data channel error", zap.Error(err)); cleanup() })
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleControlMessage(channel, message)
	})
}

func (p *peer) setInputChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting input data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.inputMu.Lock()
	previous := p.input
	p.inputChannelGeneration++
	channelGeneration := p.inputChannelGeneration
	p.input = channel
	p.inputSequence = 0
	p.inputSequenceSet = false
	p.inputLeaseGeneration++
	p.inputMu.Unlock()
	if previous != nil {
		_ = p.service.input.Release(p.id)
		_ = previous.Close()
	}

	channel.OnOpen(func() {
		p.logger.Info("input data channel opened")
	})
	cleanup := func() {
		p.inputMu.Lock()
		owned := p.input == channel && p.inputChannelGeneration == channelGeneration
		if owned {
			p.input = nil
			p.inputLeaseGeneration++
		}
		p.inputMu.Unlock()
		if owned {
			_ = p.service.input.Release(p.id)
		}
	}
	channel.OnClose(func() { cleanup() })
	channel.OnError(func(err error) { p.logger.Debug("input data channel error", zap.Error(err)); cleanup() })
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleInputMessage(channel, message, false)
	})
}

func (p *peer) setInputMotionChannel(channel *pion.DataChannel) {
	maxRetransmits := channel.MaxRetransmits()
	if channel.Ordered() || channel.MaxPacketLifeTime() != nil || maxRetransmits == nil || *maxRetransmits != 0 {
		p.logger.Info("rejecting input motion data channel without unordered zero-retransmit delivery")
		_ = channel.Close()
		return
	}

	p.inputMu.Lock()
	previous := p.inputMotion
	p.inputMotionGeneration++
	channelGeneration := p.inputMotionGeneration
	p.inputMotion = channel
	p.inputMotionSequence = 0
	p.inputMotionSequenceSet = false
	p.inputLeaseGeneration++
	p.inputMu.Unlock()
	if previous != nil {
		_ = p.service.input.Release(p.id)
		_ = previous.Close()
	}

	channel.OnOpen(func() {
		p.logger.Info("input motion data channel opened")
	})
	cleanup := func() {
		p.inputMu.Lock()
		owned := p.inputMotion == channel && p.inputMotionGeneration == channelGeneration
		if owned {
			p.inputMotion = nil
			p.inputLeaseGeneration++
		}
		p.inputMu.Unlock()
		if owned {
			_ = p.service.input.Release(p.id)
		}
	}
	channel.OnClose(func() { cleanup() })
	channel.OnError(func(err error) { p.logger.Debug("input motion data channel error", zap.Error(err)); cleanup() })
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleInputMessage(channel, message, true)
	})
}

func (p *peer) handleControlMessage(channel *pion.DataChannel, message pion.DataChannelMessage) {
	p.controlMu.Lock()
	current := p.control == channel
	p.controlMu.Unlock()
	if !current || p.isClosing() {
		return
	}
	if !message.IsString {
		p.writeControlError(channel, "", "invalid_message", "control messages must be text")
		return
	}
	if len(message.Data) > maxControlMessageBytes {
		p.writeControlError(channel, "", "message_too_large", "control message exceeds 16384 bytes")
		return
	}
	if !utf8.Valid(message.Data) {
		p.writeControlError(channel, "", "invalid_message", "control message is not valid UTF-8")
		return
	}

	request, err := decodeControlRequest(message.Data)
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "invalid_message", fmt.Sprintf("decode control message: %v", err))
		return
	}
	if protocolErr := validateControlRequest(request); protocolErr != nil {
		p.writeControlError(channel, request.ID.Value, protocolErr.Code, protocolErr.Message)
		return
	}

	switch request.Type.Value {
	case controlTypeTargetSelect:
		targeted, ok := p.service.source.(TargetMediaSource)
		if !ok {
			p.writeControlError(channel, request.ID.Value, "target_selection_disabled", "media source does not support target selection")
			return
		}
		if !p.connected.Load() {
			p.writeControlError(channel, request.ID.Value, "peer_not_connected", "WebRTC peer is not connected")
			return
		}
		requestID := request.ID.Value
		targetID := request.TargetID.Value
		generation := p.targetGeneration.Add(1)
		p.inputMu.Lock()
		p.inputLeaseGeneration++
		p.inputMu.Unlock()
		_ = p.service.input.Release(p.id)
		p.videoNeedsKeyframe.Store(true)
		p.videoSamples.clear()
		p.goOwned(func() {
			selection, err := targeted.SelectTarget(p.ctx, p.id, generation, targetID)
			p.controlMu.Lock()
			currentControl := p.control == channel
			p.controlMu.Unlock()
			if !currentControl || p.isClosing() {
				return
			}
			if err != nil {
				p.writeControlError(channel, requestID, "target_unavailable", err.Error())
				return
			}
			if !p.writeControl(channel, controlResponse{
				Version: controlVersion,
				ID:      requestID,
				Type:    controlTypeTargetSelectResult,
				OK:      true,
				Target:  &selection,
			}) {
				p.Close()
			}
		})
		return
	case controlTypeInputAcquire:
		p.controlMu.Lock()
		currentControl := p.control == channel
		p.controlMu.Unlock()
		if !currentControl || p.isClosing() {
			return
		}
		if !p.connected.Load() {
			p.writeControlError(channel, request.ID.Value, "peer_not_connected", "WebRTC peer is not connected")
			return
		}
		p.inputMu.Lock()
		inputChannel := p.input
		inputMotionChannel := p.inputMotion
		p.inputMu.Unlock()
		if inputChannel == nil || inputChannel.ReadyState() != pion.DataChannelStateOpen ||
			inputMotionChannel == nil || inputMotionChannel.ReadyState() != pion.DataChannelStateOpen {
			p.writeControlError(channel, request.ID.Value, "input_channel_required", "open reliable input and unreliable input-motion data channels are required")
			return
		}
		p.inputMu.Lock()
		inputGeneration := p.inputChannelGeneration
		inputMotionGeneration := p.inputMotionGeneration
		leaseGeneration := p.inputLeaseGeneration + 1
		p.inputLeaseGeneration = leaseGeneration
		inputStillCurrent := p.input == inputChannel && p.inputMotion == inputMotionChannel
		p.inputMu.Unlock()
		if !inputStillCurrent || p.isClosing() {
			return
		}
		capabilities, err := p.service.input.Acquire(p.id, func(sequence uint64, cause error) {
			p.onInputRevoked(inputGeneration, inputMotionGeneration, leaseGeneration, sequence, cause)
		})
		p.controlMu.Lock()
		controlStillCurrent := p.control == channel
		p.controlMu.Unlock()
		p.inputMu.Lock()
		inputStillCurrent = p.input == inputChannel &&
			p.inputMotion == inputMotionChannel &&
			p.inputChannelGeneration == inputGeneration &&
			p.inputMotionGeneration == inputMotionGeneration &&
			p.inputLeaseGeneration == leaseGeneration
		p.inputMu.Unlock()
		if err == nil && (!controlStillCurrent || !inputStillCurrent || p.isClosing()) {
			p.inputMu.Lock()
			p.inputLeaseGeneration++
			p.inputMu.Unlock()
			_ = p.service.input.Release(p.id)
			return
		}
		if err != nil {
			if !controlStillCurrent || p.isClosing() {
				return
			}
			code := inputErrorCode(err)
			p.writeControlError(channel, request.ID.Value, code, err.Error())
			return
		}
		if !p.writeControl(channel, controlResponse{
			Version: controlVersion,
			ID:      request.ID.Value,
			Type:    controlTypeInputAcquireResult,
			OK:      true,
			Input: &controlInput{
				Pointer:  capabilities.Pointer,
				Keyboard: capabilities.Keyboard,
				Text:     capabilities.Text,
			},
		}) {
			p.Close()
			return
		}
		if p.service.clipboard.Enabled() {
			p.goOwned(p.sendLatestClipboard)
		}
		return
	case controlTypeInputRelease:
		p.controlMu.Lock()
		currentControl := p.control == channel
		p.controlMu.Unlock()
		if !currentControl || p.isClosing() {
			return
		}
		if err := p.service.input.Release(p.id); err != nil {
			p.writeControlError(channel, request.ID.Value, inputErrorCode(err), err.Error())
			return
		}
		if !p.writeControl(channel, controlResponse{
			Version: controlVersion,
			ID:      request.ID.Value,
			Type:    controlTypeInputReleaseResult,
			OK:      true,
		}) {
			p.Close()
		}
		return
	}

	p.controlMu.Lock()
	qualityControl := p.control == channel
	p.controlMu.Unlock()
	if !qualityControl || p.isClosing() {
		return
	}
	p.service.qualityChangeMu.Lock()
	p.controlMu.Lock()
	qualityControl = p.control == channel
	p.controlMu.Unlock()
	if !qualityControl || p.isClosing() {
		p.service.qualityChangeMu.Unlock()
		return
	}
	qualityCurrent := p.service.source.Quality()
	quality := qualityCurrent
	profileName := qualityCurrent.Profile
	if request.Quality.Value.Profile.Set {
		profileName = request.Quality.Value.Profile.Value
	}
	profile, exists := p.service.source.Profile(profileName)
	if !exists {
		p.service.qualityChangeMu.Unlock()
		p.writeControlError(channel, request.ID.Value, "quality_update_failed", fmt.Sprintf("video profile %q is not configured", profileName))
		return
	}
	optionName := qualityCurrent.Option
	if request.Quality.Value.Profile.Set && !request.Quality.Value.Option.Set {
		optionName = profile.DefaultOption
	}
	if request.Quality.Value.Option.Set {
		optionName = request.Quality.Value.Option.Value
	}
	if request.Quality.Value.Profile.Set || request.Quality.Value.Option.Set {
		option, exists := profile.Options[optionName]
		if !exists {
			p.service.qualityChangeMu.Unlock()
			p.writeControlError(channel, request.ID.Value, "quality_update_failed", fmt.Sprintf("video option %q is not configured for profile %q", optionName, profileName))
			return
		}
		quality = option.Quality(profileName, optionName)
	}
	if request.Quality.Value.Width.Set {
		quality.Width = request.Quality.Value.Width.Value
	}
	if request.Quality.Value.Height.Set {
		quality.Height = request.Quality.Value.Height.Value
	}
	if request.Quality.Value.Framerate.Set {
		quality.Framerate = request.Quality.Value.Framerate.Value
	}
	if request.Quality.Value.BitrateKbps.Set {
		quality.BitrateKbps = request.Quality.Value.BitrateKbps.Value
	}
	result, err := p.service.updateQualityLocked(quality, p)
	p.service.qualityChangeMu.Unlock()
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "quality_update_failed", err.Error())
		return
	}
	responseWritten := p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      request.ID.Value,
		Type:    controlTypeQualitySetResult,
		OK:      true,
		Quality: qualityResponse(result.effective),
	})
	for _, candidate := range result.incompatiblePeers {
		p.service.closePeerForProfileChange(candidate, result.generation)
	}
	if !responseWritten {
		p.Close()
		return
	}
	if result.requesterNeedsReconnect {
		p.Close()
	}
}

func (p *peer) handleInputMessage(channel *pion.DataChannel, message pion.DataChannelMessage, motion bool) {
	p.inputMu.Lock()
	current := p.input == channel
	if motion {
		current = p.inputMotion == channel
	}
	p.inputMu.Unlock()
	if !current || p.isClosing() {
		return
	}
	p.inputMessagesSeen.Add(1)
	if !message.IsString {
		p.writeInputError(channel, nil, "invalid_message", "input messages must be text")
		return
	}
	if len(message.Data) > maxInputMessageBytes {
		p.writeInputError(channel, nil, "message_too_large", "input message exceeds 4096 bytes")
		return
	}
	if !utf8.Valid(message.Data) {
		p.writeInputError(channel, nil, "invalid_message", "input message is not valid UTF-8")
		return
	}

	request, err := decodeInputRequest(message.Data)
	sequence := inputSequencePointer(request)
	if err != nil {
		p.writeInputError(channel, sequence, "invalid_message", fmt.Sprintf("decode input message: %v", err))
		return
	}
	event, protocolErr := validateInputRequest(request)
	if protocolErr != nil {
		p.writeInputError(channel, sequence, protocolErr.Code, protocolErr.Message)
		return
	}
	if motion && event.Type != InputEventPointerAbsolute && event.Type != InputEventPointerRelative {
		p.writeInputError(channel, sequence, "unsupported_type", "input-motion accepts pointer motion messages only")
		return
	}

	p.inputMu.Lock()
	inputGeneration := p.inputChannelGeneration
	inputSequence := p.inputSequence
	inputSequenceSet := p.inputSequenceSet
	if motion {
		inputGeneration = p.inputMotionGeneration
		inputSequence = p.inputMotionSequence
		inputSequenceSet = p.inputMotionSequenceSet
	}
	current = p.input == channel
	if motion {
		current = p.inputMotion == channel
	}
	if !current || p.isClosing() {
		p.inputMu.Unlock()
		return
	}
	if inputSequenceSet && request.Sequence.Value <= inputSequence {
		p.inputMu.Unlock()
		if !motion {
			p.writeInputError(channel, sequence, "invalid_sequence", "sequence must increase monotonically")
		}
		return
	}
	if motion {
		p.inputMotionSequence = request.Sequence.Value
		p.inputMotionSequenceSet = true
	} else {
		p.inputSequence = request.Sequence.Value
		p.inputSequenceSet = true
	}
	p.inputMu.Unlock()

	if !p.connected.Load() {
		p.writeInputError(channel, sequence, "peer_not_connected", "WebRTC peer is not connected")
		return
	}
	if p.isClosing() {
		return
	}
	p.inputMu.Lock()
	currentInput := p.input == channel && p.inputChannelGeneration == inputGeneration
	if motion {
		currentInput = p.inputMotion == channel && p.inputMotionGeneration == inputGeneration
	}
	currentInput = currentInput && !p.isClosing()
	p.inputMu.Unlock()
	if !currentInput {
		return
	}
	if !p.service.input.Owns(p.id) {
		p.writeInputError(channel, sequence, "input_not_owned", "peer does not own input")
		return
	}
	if err := p.service.input.Submit(p.id, event); err != nil {
		if errors.Is(err, ErrInputOverloaded) {
			p.inputOverloads.Add(1)
			return
		}
		p.writeInputError(channel, sequence, inputErrorCode(err), err.Error())
		return
	}
	p.inputMessagesSent.Add(1)
}

func (p *peer) writeControlError(channel *pion.DataChannel, id, code, message string) {
	if !p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      id,
		Type:    controlTypeError,
		OK:      false,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	}) {
		p.Close()
	}
}

func (p *peer) writeControl(channel *pion.DataChannel, response controlResponse) bool {
	data, err := json.Marshal(response)
	if err != nil {
		p.logger.Error("encode control response", zap.Error(err))
		return false
	}

	p.controlMu.Lock()
	current := p.control == channel
	p.controlMu.Unlock()
	if !current || p.isClosing() {
		return false
	}
	p.controlWriteMu.Lock()
	defer p.controlWriteMu.Unlock()
	if err := channel.SendText(string(data)); err != nil {
		p.logger.Debug("control data channel write stopped", zap.Error(err))
		return false
	}
	return true
}

func (p *peer) onInputRevoked(inputGeneration, inputMotionGeneration, leaseGeneration, sequence uint64, cause error) {
	p.inputMu.Lock()
	channel := p.input
	motionChannel := p.inputMotion
	current := channel != nil &&
		motionChannel != nil &&
		p.inputChannelGeneration == inputGeneration &&
		p.inputMotionGeneration == inputMotionGeneration &&
		p.inputLeaseGeneration == leaseGeneration
	if current && !p.isClosing() {
		p.input = nil
		p.inputMotion = nil
		p.inputChannelGeneration++
		p.inputMotionGeneration++
		p.inputLeaseGeneration++
	}
	p.inputMu.Unlock()
	if !current || p.isClosing() {
		return
	}
	var correlation *uint64
	if sequence != 0 {
		correlation = &sequence
	}
	p.writeInputError(channel, correlation, inputErrorCode(cause), cause.Error())
	_ = channel.Close()
	_ = motionChannel.Close()
}

func (p *peer) writeInputError(channel *pion.DataChannel, sequence *uint64, code, message string) {
	if p.isClosing() {
		return
	}
	data, err := json.Marshal(inputResponse{
		Version:  inputVersion,
		Sequence: sequence,
		Type:     inputTypeError,
		OK:       false,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	})
	if err != nil {
		p.logger.Error("encode input response", zap.Error(err))
		return
	}

	p.inputWriteMu.Lock()
	defer p.inputWriteMu.Unlock()
	if err := channel.SendText(string(data)); err != nil {
		p.logger.Debug("input data channel write stopped", zap.Error(err))
	}
}

func inputSequencePointer(request inputRequest) *uint64 {
	if !request.Sequence.Set {
		return nil
	}
	sequence := request.Sequence.Value
	return &sequence
}

func inputErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrInputBusy):
		return "input_busy"
	case errors.Is(err, ErrInputDisabled):
		return "input_disabled"
	case errors.Is(err, ErrInputPointerUnauthorized):
		return "input_pointer_unauthorized"
	case errors.Is(err, ErrInputKeyboardUnauthorized):
		return "input_keyboard_unauthorized"
	case errors.Is(err, ErrInputNotReady):
		return "input_not_ready"
	case errors.Is(err, ErrInputNotOwner):
		return "input_not_owned"
	case errors.Is(err, ErrInputOverloaded):
		return "input_overloaded"
	case errors.Is(err, ErrInputClosed):
		return "input_unavailable"
	default:
		return "input_failed"
	}
}
