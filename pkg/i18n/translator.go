// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Milos Vasic

// Package i18n defines the Translator contract containers' user-facing
// error messages, status descriptions, and operator-visible text use to
// externalise message text per CONST-046 (no-hardcoded-content mandate
// cascaded via constitution submodule §11.4.36).
//
// The package intentionally avoids any import of consumer-project paths
// (CONST-051(B) decoupling mandate) — containers stays standalone and
// reusable; any consuming project may supply its own Translator
// implementation that loads bundles, calls an LLM, or composes from
// configuration at runtime.
//
// Round 129 of the project's CONST-046 sweep introduced this contract
// to externalise the most-frequently-end-user-visible literals embedded
// across the distribution workflow phase descriptions and the buildpkg
// executor error returns. Those strings travel verbatim into operator-
// facing error returns and progress descriptions surfaced to API
// consumers; the previous hardcoding meant a Serbian or Japanese end
// user would receive an English error body and English phase narration
// regardless of any `Accept-Language` negotiation the consuming project
// performed.
package i18n

import "context"

// Translator is the contract every i18n implementation must satisfy.
//
// T returns the localised rendering of msgID with named arguments
// substituted (`{{.key}}` style at the implementation's discretion).
//
// TPlural returns the localised rendering of msgID using plural-form
// resolution against count (CLDR Cardinal rules at the implementation's
// discretion).
type Translator interface {
	T(ctx context.Context, msgID string, args map[string]any) string
	TPlural(ctx context.Context, msgID string, count int, args map[string]any) string
}

// NoopTranslator is the default Translator returned when no other
// implementation is wired. It returns the message ID verbatim so the
// containers surface remains functional in stripped-down environments
// (unit tests, integration harnesses that exercise wire format only)
// and so absence-of-bundle is loudly visible in captured error
// responses.
//
// Per CONST-035 / Article XI §11.9 the verbatim-ID fallback is itself
// positive evidence — operators see exactly which key failed to
// resolve rather than an opaque empty string. End users still receive
// a non-empty message with a recognisable shape (`containers_…` prefix)
// distinguishable from a blank wire error.
type NoopTranslator struct{}

// T returns msgID unchanged.
func (NoopTranslator) T(_ context.Context, msgID string, _ map[string]any) string {
	return msgID
}

// TPlural returns msgID unchanged.
func (NoopTranslator) TPlural(_ context.Context, msgID string, _ int, _ map[string]any) string {
	return msgID
}
