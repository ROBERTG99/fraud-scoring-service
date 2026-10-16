package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
)

func TestTokenVerifiesAgainstPublishedJWKS(t *testing.T) {
	signer, err := auth.NewSigner("http://dev-issuer:8515")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(signer))
	defer srv.Close()
	v := &auth.Verifier{Issuer: signer.Issuer, Keys: &auth.RemoteKeys{URL: srv.URL + "/.well-known/jwks.json"}}
	for _, p := range Personas {
		resp, err := http.Post(srv.URL+"/token", "application/json", bytes.NewBufferString(`{"persona":"`+p.ID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		id, err := v.Verify(context.Background(), body.AccessToken)
		if err != nil || id.Subject != p.ID || id.Role != p.Role {
			t.Fatalf("%s: %+v %v", p.ID, id, err)
		}
	}
	for body, want := range map[string]int{`{"persona":"root"}`: 404, `nope`: 400} {
		resp, err := http.Post(srv.URL+"/token", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: status %d want %d", body, resp.StatusCode, want)
		}
	}
	resp, _ := http.Get(srv.URL + "/personas")
	var ps struct {
		Personas []Persona `json:"personas"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ps)
	resp.Body.Close()
	if len(ps.Personas) != 3 {
		t.Fatalf("personas %v", ps)
	}
}
