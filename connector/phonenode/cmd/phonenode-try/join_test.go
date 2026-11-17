package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A join that fails before the network says so without the material.
func TestJoinRefusesBadMaterialWithoutPrintingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := writePaired(dir, paired{
		Origin:      "https://mcp.example",
		PhoneName:   "harness",
		AccessToken: "bearer-secret-xyz",
		Enrollment:  []byte(`{"enrollment_id":"e1","preauth_key":"pk-secret-xyz","head_url":"https://head.invalid"}`),
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := joinCommand([]string{"--state", dir}, &out)
	if err == nil {
		t.Fatalf("join took material with no nonce:\n%s", out.String())
	}
	noSecrets(t, "the output", out.String()+err.Error())
}

func TestJoinWithSpentMaterialSaysRunDoor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := writePaired(dir, paired{Origin: "https://mcp.example", PhoneName: "harness"}); err != nil {
		t.Fatal(err)
	}
	err := joinCommand([]string{"--state", dir}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "run door") {
		t.Fatalf("join = %v", err)
	}
}

// After a join the pre-auth key is spent; a phone.json that cannot be
// rewritten without it still holds it, and the error says so and what next.
func TestSpendEnrollmentThatCannotWriteSaysTheMaterialWasLeft(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p := paired{
		Origin:      "https://mcp.example",
		PhoneName:   "harness",
		AccessToken: "bearer-secret-xyz",
		Enrollment:  []byte(`{"enrollment_id":"e1","preauth_key":"pk-secret-xyz"}`),
	}
	if err := writePaired(dir, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := spendEnrollment(dir, p)
	if err == nil || !strings.Contains(err.Error(), "the spent join material was left in phone.json") ||
		!strings.Contains(err.Error(), "run door") {
		t.Fatalf("spendEnrollment = %v", err)
	}
	noSecrets(t, "the error", err.Error())
	if left, _ := readPaired(dir); len(left.Enrollment) == 0 {
		t.Fatal("the test meant phone.json to keep the material, and it did not")
	}
}

func TestSpendEnrollmentDropsTheMaterial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p := paired{Origin: "https://mcp.example", PhoneName: "harness", Enrollment: []byte(`{"preauth_key":"pk-secret-xyz"}`)}
	if err := spendEnrollment(dir, p); err != nil {
		t.Fatal(err)
	}
	if left, err := readPaired(dir); err != nil || len(left.Enrollment) != 0 || left.PhoneName != "harness" {
		t.Fatalf("phone.json = %+v, %v", left, err)
	}
}
