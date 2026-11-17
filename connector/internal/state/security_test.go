package state

import "testing"

func TestRecordGatewayRefusesNonOverlay(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	original := enrolled()
	if err := store.SaveNode(original); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"127.0.0.1", "::1", "192.0.2.10", "8.8.8.8", "2001:4860:4860::8888", "fd06:6e8::1", "fd06:6e7::1%en0", "invalid", ""} {
		if err := store.RecordGateway(address, 443, "replacement"); err == nil {
			t.Errorf("accepted %q", address)
		}
		got, err := store.LoadNode()
		if err != nil || got != original {
			t.Fatalf("refusal changed state for %q: %v", address, err)
		}
	}
}
