package opencodeacp

import (
	"context"
	"errors"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

func validateHostAuthority(authority HostAuthority) (err error) {
	_, err = hostAuthorityEnvironment(authority)

	return err
}

func hostAuthorityEnvironment(authority HostAuthority) (environment map[string]string, err error) {
	if authority == nil {
		return nil, ErrHostAuthorityUnavailable
	}

	defer func() {
		if recover() != nil {
			err = ErrHostAuthorityUnavailable
			environment = nil
		}
	}()

	environment = authority.NativeEnvironment()
	if environment == nil {
		return nil, ErrHostAuthorityUnavailable
	}

	if err := opencode.ValidateEnvironment(environment); err != nil {
		return nil, errors.Join(ErrHostAuthorityUnavailable, err)
	}

	return cloneStringMap(environment), nil
}

func authorityProcessStarter(authority HostAuthority) opencode.ProcessStarter {
	return func(ctx context.Context, executable string, arguments, environment []string, workingDirectory string) (handle opencode.ProcessHandle, err error) {
		defer func() {
			if recover() != nil {
				handle = opencode.ProcessHandle{}
				err = ErrHostAuthorityUnavailable
			}
		}()

		process, err := authority.StartNative(ctx, NativeRequest{
			Executable:       executable,
			Arguments:        append([]string(nil), arguments...),
			Environment:      append([]string(nil), environment...),
			WorkingDirectory: workingDirectory,
		})
		if err != nil {
			return opencode.ProcessHandle{}, errors.Join(ErrHostAuthorityUnavailable, err)
		}

		if process == nil {
			return opencode.ProcessHandle{}, ErrHostAuthorityUnavailable
		}

		stdin, stdout, stderr := process.Stdin(), process.Stdout(), process.Stderr()
		if stdin == nil || stdout == nil || stderr == nil {
			return opencode.ProcessHandle{}, ErrHostAuthorityUnavailable
		}

		return opencode.ProcessHandle{
			Input: stdin, Output: stdout, Errors: stderr,
			Await: func(waitCtx context.Context) (outcome opencode.ProcessOutcome, waitErr error) {
				defer func() {
					if recover() != nil {
						outcome = opencode.ProcessOutcome{}
						waitErr = ErrContainmentIncomplete
					}
				}()

				result, waitErr := process.Wait(waitCtx)

				outcome = opencode.ProcessOutcome{
					ExitCode: result.ExitCode,
					Signal:   result.Signal,
					Revoked:  result.Revoked,
				}

				if waitErr != nil {
					waitErr = errors.Join(ErrContainmentIncomplete, waitErr)
				}

				return outcome, waitErr
			},
			Stop: func(revokeCtx context.Context) (revokeErr error) {
				defer func() {
					if recover() != nil {
						revokeErr = ErrContainmentIncomplete
					}
				}()

				if revokeErr := process.Revoke(revokeCtx); revokeErr != nil {
					return errors.Join(ErrContainmentIncomplete, revokeErr)
				}

				return nil
			},
		}, nil
	}
}
