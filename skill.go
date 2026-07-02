// Package klimax exposes repo-root assets that are compiled into the binary.
package klimax

import _ "embed"

// SkillMD is the canonical klimax Agent Skill definition, embedded from the
// repo-root SKILL.md. `klimax skill install` ships it inside the binary so the
// installed CLI can drop the skill into an AI coding tool's skills directory
// without a separate download. SKILL.md remains the single source of truth.
//
//go:embed SKILL.md
var SkillMD string
