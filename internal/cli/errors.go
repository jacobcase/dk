package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jacobcase/dk/internal/auth"
	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// Exit codes. Distinct codes let a calling script or agent branch on the
// failure mode without parsing the message.
const (
	ExitOK        = 0
	ExitError     = 1 // generic runtime failure
	ExitUsage     = 2 // bad flags or arguments
	ExitAuth      = 3 // credentials missing, expired, or rejected
	ExitNotFound  = 4 // the requested product or list does not exist
	ExitRateLimit = 5 // DigiKey returned 429
	ExitConfig    = 6 // configuration is missing or invalid
)

// Machine-readable error codes emitted in JSON error output.
const (
	CodeError       = "error"
	CodeUsage       = "usage_error"
	CodeAuth        = "auth_required"
	CodeCredentials = "credentials_missing"
	CodeNotFound    = "not_found"
	CodeRateLimit   = "rate_limited"
	CodeAPI         = "api_error"
	CodeAmbiguous   = "ambiguous_list"
	CodeCancelled   = "cancelled"
	// CodeConfig is a config file that exists but cannot be used. Distinct from
	// CodeCredentials, which means the file is fine and a field is missing.
	CodeConfig = "config_invalid"
)

// Error carries an exit code and a machine-readable code alongside the message.
// Every command failure should be wrapped in one so the top-level handler can
// render it consistently.
type Error struct {
	Code     string
	Message  string
	Hint     string
	ExitCode int
	Err      error
	// Details carries structured extras (validation errors, candidate lists)
	// into JSON error output.
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// usageErrorf builds an Error for bad user input.
func usageErrorf(format string, args ...any) *Error {
	return &Error{Code: CodeUsage, Message: fmt.Sprintf(format, args...), ExitCode: ExitUsage}
}

// classify maps an arbitrary error onto an *Error with the right exit code.
// Errors that are already *Error pass through untouched.
func classify(err error) *Error {
	if err == nil {
		return nil
	}

	var cliErr *Error
	if errors.As(err, &cliErr) {
		return cliErr
	}

	// Ctrl-C. Classified like any other failure so JSON callers still get a
	// structured object on stderr rather than a line of prose.
	if errors.Is(err, context.Canceled) {
		return &Error{
			Code:     CodeCancelled,
			Message:  "cancelled",
			ExitCode: ExitError,
			Err:      err,
		}
	}

	if errors.Is(err, auth.ErrLoginRequired) {
		return &Error{
			Code:     CodeAuth,
			Message:  err.Error(),
			Hint:     "Run `dk auth login` once in an interactive terminal. MyLists requires a 3-legged token tied to your DigiKey account.",
			ExitCode: ExitAuth,
			Err:      err,
		}
	}

	// An empty list argument is a bad invocation, not a missing list.
	if errors.Is(err, digikey.ErrListRefRequired) {
		return &Error{
			Code:     CodeUsage,
			Message:  err.Error(),
			Hint:     "Pass a list name or id. Run `dk list ls` to see what exists.",
			ExitCode: ExitUsage,
			Err:      err,
		}
	}

	if errors.Is(err, digikey.ErrListNotFound) {
		return &Error{
			Code:     CodeNotFound,
			Message:  err.Error(),
			Hint:     "Run `dk list ls` to see available lists, or `dk list create <name>` to make one.",
			ExitCode: ExitNotFound,
			Err:      err,
		}
	}

	var ambiguous *digikey.ErrAmbiguousList
	if errors.As(err, &ambiguous) {
		candidates := make([]map[string]any, 0, len(ambiguous.Candidate))
		for _, c := range ambiguous.Candidate {
			candidates = append(candidates, map[string]any{"id": c.ID, "name": c.ListName})
		}
		return &Error{
			Code:     CodeAmbiguous,
			Message:  err.Error(),
			Hint:     "Pass the list id instead of the name.",
			ExitCode: ExitUsage,
			Err:      err,
			Details:  map[string]any{"candidates": candidates},
		}
	}

	var oauthErr *auth.Error
	if errors.As(err, &oauthErr) {
		return &Error{
			Code:     CodeAuth,
			Message:  oauthErr.Error(),
			Hint:     "Check DIGIKEY_CLIENT_ID / DIGIKEY_CLIENT_SECRET, and that your app is subscribed to the API on developer.digikey.com.",
			ExitCode: ExitAuth,
			Err:      err,
		}
	}

	var apiErr *digikey.APIError
	if errors.As(err, &apiErr) {
		return classifyAPIError(apiErr)
	}

	return &Error{Code: CodeError, Message: err.Error(), ExitCode: ExitError, Err: err}
}

func classifyAPIError(apiErr *digikey.APIError) *Error {
	e := &Error{Code: CodeAPI, Message: apiErr.Error(), ExitCode: ExitError, Err: apiErr}
	if apiErr.RequestID != "" {
		e.Details = map[string]any{"request_id": apiErr.RequestID}
	}
	if len(apiErr.ValidationErrors) > 0 {
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		e.Details["validation_errors"] = apiErr.ValidationErrors
	}

	switch {
	case apiErr.Unauthorized():
		e.Code = CodeAuth
		e.ExitCode = ExitAuth
		e.Hint = "The token was rejected. Confirm your app is subscribed to this API on developer.digikey.com; for MyLists, run `dk auth login` again."
	case apiErr.NotFound():
		e.Code = CodeNotFound
		e.ExitCode = ExitNotFound
	case apiErr.RateLimited():
		e.Code = CodeRateLimit
		e.ExitCode = ExitRateLimit
		e.Hint = "DigiKey rate limit exceeded."
		if apiErr.RetryAfter > 0 {
			e.Hint = fmt.Sprintf("DigiKey rate limit exceeded; retry after %s.", apiErr.RetryAfter)
			if e.Details == nil {
				e.Details = map[string]any{}
			}
			e.Details["retry_after_seconds"] = int(apiErr.RetryAfter.Seconds())
		}
	}
	return e
}

// writeError renders an error to w. In JSON mode it emits a structured object
// so an agent can branch on `.error.code` without regex-matching prose.
func writeError(w io.Writer, format output.Format, e *Error) {
	if format == output.FormatJSON {
		errObj := map[string]any{
			"code":      e.Code,
			"message":   e.Message,
			"exit_code": e.ExitCode,
		}
		if e.Hint != "" {
			errObj["hint"] = e.Hint
		}
		if len(e.Details) > 0 {
			errObj["details"] = e.Details
		}
		payload := map[string]any{"error": errObj}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload); err == nil {
			return
		}
		// Fall through to plain text if the payload somehow fails to encode.
	}

	fmt.Fprintf(w, "Error: %s\n", strings.TrimSpace(e.Message))
	if e.Hint != "" {
		fmt.Fprintf(w, "Hint: %s\n", e.Hint)
	}
}
