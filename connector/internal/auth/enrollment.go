package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

const (
	// enrollmentPath hangs off the MCP origin, beside the device flow's two.
	enrollmentPath = "/oauth/enrollment"

	// maxEnrollmentBody caps the enrollment answer. It carries the gateway's
	// CA certificate as well as the key, so it is larger than an OAuth
	// answer; Desk reads the same endpoint with the same cap.
	maxEnrollmentBody = 64 << 10
)

// The refusals the enrollment endpoint gives that a person can act on, each
// with its own remedy, so a caller says what to do next without knowing what
// a status code means.
var (
	ErrNoGateway       = errors.New("none of your organisations has a 6net gateway yet")
	ErrHeadUnavailable = errors.New("the 6net head is busy; try again in a minute")
	ErrSessionExpired  = errors.New("your session is no longer valid — run `gentility login`")
	ErrNotDeviceLogin  = errors.New("this login cannot enrol a machine")
)

// FetchEnrollment mints this machine's join material for the person whose
// bearer it is, at the moment it joins: POST <origin>/oauth/enrollment.
//
// The material is fetched here rather than at login for two reasons. The
// pre-auth key in it dies ten minutes after it is minted, so material taken at
// login is dead by the time anyone gets round to joining; and a person who is
// already logged in has no login to take it from. Desk asks the same door the
// same way when its Join button is pressed.
//
// The answer holds a pre-auth key and a nonce, so a 200 is never quoted back,
// even when it is not the shape expected. A nil client means one bounded by
// requestTimeout.
func FetchEnrollment(ctx context.Context, client *http.Client, origin, bearer, label string) (state.Enrollment, error) {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	endpoint := origin + enrollmentPath

	body, err := json.Marshal(map[string]string{"machine_label": label})
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("building the request to %s: %w", endpoint, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("building the request to %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)

	status, answer, err := send(client, req, endpoint, maxEnrollmentBody)
	if err != nil {
		return state.Enrollment{}, err
	}

	if status == http.StatusOK {
		var granted struct {
			Enrollment *state.Enrollment `json:"enrollment"`
		}
		if json.Unmarshal(answer, &granted) != nil || granted.Enrollment == nil || granted.Enrollment.PreauthKey == "" {
			return state.Enrollment{}, fmt.Errorf("%s answered 200 without join material", endpoint)
		}
		return *granted.Enrollment, nil
	}

	var refusal tokenResponse
	json.Unmarshal(answer, &refusal)

	switch {
	case status == http.StatusUnauthorized:
		return state.Enrollment{}, ErrSessionExpired
	case status == http.StatusForbidden:
		return state.Enrollment{}, ErrNotDeviceLogin
	case status == http.StatusConflict && refusal.Error == "no_gateway":
		return state.Enrollment{}, ErrNoGateway
	case status == http.StatusServiceUnavailable:
		return state.Enrollment{}, ErrHeadUnavailable
	case refusal.ErrorDescription != "":
		return state.Enrollment{}, fmt.Errorf("%s answered %d: %s", endpoint, status, refusal.ErrorDescription)
	default:
		return state.Enrollment{}, fmt.Errorf("%s answered %d: %s", endpoint, status, snippet(answer))
	}
}
