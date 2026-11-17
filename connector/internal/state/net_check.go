package state

import (
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

const netCheckFile = "net-check.json"

// NetCheck contains private peer endpoints. The daemon writes this with the
// same owner-only permissions as its other private state, never as status.
type NetCheck struct {
	WrittenAt  time.Time               `json:"written_at"`
	Peers      []tsnode.PeerDiagnostic `json:"peers"`
	Incomplete bool                    `json:"incomplete,omitempty"`
}

func (s *Store) SaveNetCheck(check NetCheck) error {
	return s.writeJSON(netCheckFile, check, filePerm)
}

func (s *Store) LoadNetCheck() (NetCheck, error) {
	var check NetCheck
	err := s.readJSON(netCheckFile, &check)
	return check, err
}
