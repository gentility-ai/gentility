package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// enrollmentFile holds what `connector login --server` was handed: the
// pre-exchange material a server needs to join, and nothing that identifies it
// yet. It lives only between that login and the first successful exchange —
// `serve` clears it once node.json exists — because it is a one-hour bearer of
// two secrets and a purged one is a re-enrollment, not a lost identity. Same
// directory, same 0711/0600 modes, same atomic write as the rest of the state.
const enrollmentFile = "enrollment.json"

// ErrNoEnrollment reports that this state directory holds no enrollment
// material: nobody has run `connector login --server`, or the exchange already
// consumed it. It is deliberately not ErrNotEnrolled, which is the *other* side
// of the same moment — that one means "no node credential stored", and its
// answer is to exchange, where this one's is to log in again.
var ErrNoEnrollment = errors.New("no enrollment")

// IsNoEnrollment reports whether err means "no enrollment material stored".
func IsNoEnrollment(err error) bool { return errors.Is(err, ErrNoEnrollment) }

// Enrollment is the enrollment object the token response carries, as the
// server sent it.
//
// The preauth key and the nonce are the two secrets that buy a node identity,
// so this file is written 0600 in a 0711 directory and never logged — String
// redacts both, leaving an enrollment id and a head URL and nothing else. The
// gateway trio is present only when the organization has a placed gateway.
type Enrollment struct {
	// Origin is the MCP login origin which minted this enrollment.
	Origin       string `json:"origin,omitempty"`
	EnrollmentID string `json:"enrollment_id"`
	PreauthKey   string `json:"preauth_key"`
	Nonce        string `json:"nonce"`
	HeadURL      string `json:"head_url"`
	ExpiresAt    string `json:"expires_at"`
	GatewayAddr  string `json:"gateway_addr,omitempty"`
	GatewayPort  int    `json:"gateway_port,omitempty"`
	GatewayCA    string `json:"gateway_ca,omitempty"`
}

// String renders the enrollment without either secret.
//
// The receiver is a value so that %v on an Enrollment and on an *Enrollment
// both reach it — a redaction only one of the two spellings honours is a
// redaction that will be missed.
func (e Enrollment) String() string {
	return fmt.Sprintf(
		"enrollment %s (head %s, preauth key %s, nonce %s, expires %s)",
		e.EnrollmentID, e.HeadURL, redacted(e.PreauthKey), redacted(e.Nonce), e.ExpiresAt,
	)
}

// LoadEnrollment reads the stored enrollment material. A missing file, or one
// naming no preauth key, is ErrNoEnrollment.
func (s *Store) LoadEnrollment() (Enrollment, error) {
	var enrollment Enrollment
	if err := s.readJSON(enrollmentFile, &enrollment); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Enrollment{}, ErrNoEnrollment
		}
		return Enrollment{}, err
	}
	if enrollment.PreauthKey == "" {
		return Enrollment{}, ErrNoEnrollment
	}
	return enrollment, nil
}

// SaveEnrollment writes the enrollment material, owner-readable only.
func (s *Store) SaveEnrollment(e Enrollment) error { return s.writeJSON(enrollmentFile, e, filePerm) }

// ClearEnrollment forgets the enrollment material, which `serve` does the
// moment the exchange has produced an identity. Forgetting what is already
// forgotten is not an error.
func (s *Store) ClearEnrollment() error {
	if err := os.Remove(filepath.Join(s.Dir, enrollmentFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", enrollmentFile, err)
	}
	return nil
}
