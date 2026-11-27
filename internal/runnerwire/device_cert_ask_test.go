// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The device-certificate ask rides AUTH_OK as one optional member: unset it is not sent, an old runner still reads
// the message, and an old organisation's AUTH_OK reads as no ask.
func TestDeviceCertIsOneOptionalAuthOKMember(t *testing.T) {
	on, err := Marshal(AuthOK{SessionID: "s", DeviceCert: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"session_id":"s","resumed":false,"ping_interval":0,"last_seq":0,"device_cert":true}`; string(on) != want {
		t.Fatalf("wire form %s, want %s", on, want)
	}
	var old oldAuthOK
	if err := Unmarshal(on, &old); err != nil || old.SessionID != "s" {
		t.Fatalf("an old runner decoding a new AUTH_OK = %+v, %v", old, err)
	}
	off, _ := Marshal(AuthOK{})
	if containsKey(off, "device_cert") {
		t.Fatalf("an unset device_cert is sent: %s", off)
	}
	var a AuthOK
	if err := Unmarshal([]byte(`{"session_id":"s","proxy_image":"p","local_attach":true}`), &a); err != nil || a.DeviceCert {
		t.Fatalf("an old organisation's AUTH_OK read as asking for the device certificate: %+v, %v", a, err)
	}
}

// The organisation's device-certificate ask in AUTH_OK reaches the runner's hook on every connect; an organisation
// that does not say (an older one, or one with no MDM device CA) tells it false.
func TestClientTellsTheHookWhetherTheOrganisationAsksForTheDeviceCert(t *testing.T) {
	for _, ok := range []AuthOK{{DeviceCert: true}, {}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		const orgURL = "https://org.example"
		row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
		org, run := Loopback()
		go func() {
			defer org.Close()
			if _, err := AuthenticateRunner(ctx, org, row, orgURL); err == nil {
				ok.SessionID, ok.PingInterval = uuid.NewString(), PingInterval
				_ = WriteControl(ctx, org, TypeAuthOK, ok)
			}
		}()
		var got []bool
		hooks := idleHooks()
		hooks.DeviceCert = func(on bool) { got = append(got, on) }
		client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
			Dial: func(context.Context) (Conn, error) { return run, nil }})
		if err != nil {
			t.Fatal(err)
		}
		_ = client.Connect(ctx)
		cancel()
		if len(got) != 1 || got[0] != ok.DeviceCert {
			t.Fatalf("hook saw %v, want [%v]", got, ok.DeviceCert)
		}
	}
}
