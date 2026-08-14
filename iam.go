package voice

import (
	"fmt"
	"strings"
	"time"

	"github.com/hanzoai/authz/edge"
)

// Who is the caller, as IAM attests them. Nothing here is read from a header
// the client controls: every field comes out of a signature.
type Who struct {
	User  string // subject — a person, never a key
	Org   string // the tenant that pays for the audio
	Name  string
	Token string // the caller's own bearer, carried so downstream calls bill them
}

// Gate answers who a bearer belongs to.
//
// Verification is IAM's own SDK rather than a JWT library used directly. That
// is not deference: the org a token authorises is NOT its `owner` claim —
// `owner` names the application the token was minted through, so reading it as
// the tenant bills one org for another org's audio. Claims.Home() reads the
// signed membership set instead, and it is the only correct answer.
type Gate struct{ v *edge.Verifier }

// Keys are cached for a minute: long enough that a conversation never waits on
// IAM, short enough that a rotation lands without a restart.
const keyLife = time.Minute

func NewGate(jwks string, issuers, audiences []string) *Gate {
	return &Gate{v: edge.NewVerifier(jwks, issuers, audiences, keyLife)}
}

// Read verifies a bearer and says who holds it.
//
// `selected` is the caller's X-Org-Id, honoured only when the signed membership
// set contains it — EffectiveOrg does that check, so a client cannot bill a
// tenant it does not belong to by asking nicely.
func (g *Gate) Read(bearer, selected string) (Who, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bearer), "Bearer "))
	if raw == "" {
		return Who{}, fmt.Errorf("no bearer")
	}
	claims, err := g.v.VerifyRaw(raw)
	if err != nil {
		return Who{}, fmt.Errorf("bearer rejected: %w", err)
	}
	// A machine credential carries no subject. Refusing it here is structural
	// rather than a list of credential kinds to keep up to date: audio seconds
	// have to land on a person for caller-pays billing to mean anything.
	if strings.TrimSpace(claims.Subject) == "" {
		return Who{}, fmt.Errorf("credential names no subject")
	}
	org := claims.Home()
	if selected = strings.TrimSpace(selected); selected != "" {
		if eff, _ := claims.EffectiveOrg(selected); eff != "" {
			org = eff
		}
	}
	if org == "" {
		return Who{}, fmt.Errorf("credential names no org")
	}
	return Who{User: claims.Subject, Org: org, Name: claims.Name, Token: raw}, nil
}
