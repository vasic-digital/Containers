# GEMINI.md — Gemini CLI context for this module

## INHERITED FROM the Helix Constitution

This module is governed by the Helix Constitution. All rules in the
constitution's `GEMINI.md` and the `Constitution.md` it references apply
unconditionally. Locate the constitution from any nested depth via its
`find_constitution.sh` helper — do NOT hardcode a path (this module stays
fully decoupled and project-agnostic per §11.4.28).

Canonical reference: https://github.com/HelixDevelopment/HelixConstitution

## Read CLAUDE.md — it is mandatory

This module's canonical agent-instruction file is `CLAUDE.md` in this directory.
Before doing any work in this module, open and read `CLAUDE.md` and this
module's `CONSTITUTION.md` in full. Every rule there binds Gemini CLI exactly
as it binds Claude Code.

This file is a plain-text pointer and deliberately uses no auto-import
directive. Gemini CLI's memory-import processor resolves `@`-prefixed import
paths recursively, and the instruction files reference tokens that are not
files. To stay compatible with Gemini CLI this file contains no such tokens —
read `CLAUDE.md` directly.
