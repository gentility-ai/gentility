package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/contracttest"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
)

func TestPublishedDeskCommandsAreAcceptedWithoutSideEffects(t *testing.T) {
	var contract struct {
		SuccessExit int                 `json:"success_exit"`
		Commands    map[string][]string `json:"commands"`
	}
	if err := json.Unmarshal(contracttest.Read(t, "client/cli.json"), &contract); err != nil {
		t.Fatal(err)
	}
	if len(contract.Commands) != 3 || contract.SuccessExit != exitcodes.OK {
		t.Fatal("incomplete Desk command contract")
	}
	for name, args := range contract.Commands {
		t.Run(name, func(t *testing.T) {
			// --help comes last so every advertised flag is parsed first.
			// It returns before elevation, state reads or installation.
			var stdout, stderr bytes.Buffer
			code := run(append(args, "--help"), &stdout, &stderr)
			if code != contract.SuccessExit || stdout.Len() == 0 {
				t.Fatalf("published command refused: code %d, stderr %s", code, stderr.String())
			}
		})
	}
}
