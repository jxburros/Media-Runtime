package pool

import (
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
	"github.com/jxburros/media-runtime/internal/worker"
)

func probe(t *testing.T, protocol, identity string) *worker.Probe {
	t.Helper()
	info, err := jsonv.Parse([]byte(`{"device_identity":` + identity + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return &worker.Probe{Info: info.(*jsonv.Object), Caps: jsonv.NewObject(), Protocol: protocol}
}

// The device is named by host, device and index; two processes on one device share the key.
func TestIdentityKeyNamesTheDeviceNotTheProcess(t *testing.T) {
	a := IdentityKey(probe(t, "media-worker/1.2", `{"device":"cuda:0","index":0,"host_id":"h","pid":1}`))
	b := IdentityKey(probe(t, "media-worker/1.2", `{"device":"cuda:0","index":0,"host_id":"h","pid":2}`))
	if a == "" || a != b {
		t.Fatalf("%q %q", a, b)
	}
	if IdentityKey(probe(t, "media-worker/1.2", `{"device":"cuda:1","index":1,"host_id":"h","pid":1}`)) == a {
		t.Fatal("another device has the same key")
	}
	if IdentityKey(probe(t, "media-worker/1.2", `{"device":"cuda:0","index":0,"host_id":"other","pid":1}`)) == a {
		t.Fatal("another host has the same key")
	}
	if IdentityKey(probe(t, "media-worker/1.1", `{"device":"cuda:0","index":0,"host_id":"h"}`)) != "" {
		t.Fatal("a worker before 1.2 reports no comparable identity")
	}
	if IdentityKey(probe(t, "media-worker/1.2", `null`)) != "" {
		t.Fatal("no identity, no key")
	}
}
