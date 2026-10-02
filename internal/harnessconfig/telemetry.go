package harnessconfig

import (
	"github.com/Siriusrry/cc-automux/internal/config"
)

// TelemetryUpdater prepares external-file changes without depending on Runtime
// or acquiring its lock. Validator remains a read-only projection checker.
type TelemetryUpdater struct{ validator *Validator }

func NewTelemetryUpdater(validator *Validator) *TelemetryUpdater {
	return &TelemetryUpdater{validator: validator}
}

func (u *TelemetryUpdater) PrepareTelemetry(current, next config.Config) (config.HarnessFileUpdate, config.HarnessValidation, error) {
	check, adapter, original, err := u.validator.inspect(current)
	if err != nil || check.State != string(StateInSync) {
		return nil, check, err
	}
	profile, ok := u.validator.profileFor(next, current.Harnesses.ClaudeCode.ActiveProfileID)
	if !ok {
		return nil, check, ErrProfileNotFound
	}
	projection, err := adapter.BuildManagedProjection(activationInput(next, profile))
	if err != nil {
		return nil, check, activationInputError(err)
	}
	prepared, err := u.validator.files.prepare(check.ResolvedPath, adapter, projection, original)
	if err != nil {
		return nil, check, activationFileError(err)
	}
	return telemetryWrite{prepared}, check, nil
}

type telemetryWrite struct{ file *PreparedFile }

func (w telemetryWrite) Commit() error {
	_, err := w.file.Commit()
	if err != nil {
		return activationFileError(err)
	}
	return nil
}
func (w telemetryWrite) Rollback() error { return w.file.Rollback() }
