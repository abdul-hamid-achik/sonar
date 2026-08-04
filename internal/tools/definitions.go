package tools

import "github.com/abdul-hamid-achik/sonar/internal/llm"

var builtinToolNames = map[string]bool{
	"grep":            true,
	"read":            true,
	"write":           true,
	"glob":            true,
	"bash":            true,
	"ls":              true,
	"find":            true,
	"diff":            true,
	"edit":            true,
	"mkdir":           true,
	"remove":          true,
	"copy":            true,
	"move":            true,
	"exists":          true,
	"load_skill":      true,
	"consult_experts": true,
}

func AllToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		GrepToolDef(),
		ReadToolDef(),
		WriteToolDef(),
		GlobToolDef(),
		BashToolDef(),
		LsToolDef(),
		FindToolDef(),
		DiffToolDef(),
		EditToolDef(),
		MkdirToolDef(),
		RemoveToolDef(),
		CopyToolDef(),
		MoveToolDef(),
		ExistsToolDef(),
		LoadSkillToolDef(),
		ConsultExpertsToolDef(),
	}
}

func IsBuiltinTool(name string) bool {
	return builtinToolNames[name]
}
