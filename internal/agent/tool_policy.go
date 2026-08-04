package agent

type ToolPolicy struct {
	AllowMCP    bool
	localTools  map[string]struct{}
	memoryTools map[string]struct{}
}

func NewToolPolicy(localTools, memoryTools []string, allowMCP bool) ToolPolicy {
	policy := ToolPolicy{
		AllowMCP:    allowMCP,
		localTools:  make(map[string]struct{}, len(localTools)),
		memoryTools: make(map[string]struct{}, len(memoryTools)),
	}

	for _, name := range localTools {
		policy.localTools[name] = struct{}{}
	}
	for _, name := range memoryTools {
		policy.memoryTools[name] = struct{}{}
	}

	return policy
}

func DefaultToolPolicy() ToolPolicy {
	return BuildToolPolicy()
}

func AskToolPolicy() ToolPolicy {
	return NewToolPolicy(
		[]string{"read", "grep", "glob", "ls", "find", "diff", "exists", "load_skill", "consult_experts"},
		[]string{"memory_recall"},
		false,
	)
}

func PlanToolPolicy() ToolPolicy {
	return AskToolPolicy()
}

func BuildToolPolicy() ToolPolicy {
	return NewToolPolicy(
		[]string{"grep", "read", "write", "edit", "glob", "bash", "ls", "find", "diff", "mkdir", "remove", "copy", "move", "exists", "load_skill", "consult_experts"},
		[]string{"memory_save", "memory_recall", "memory_delete", "memory_update", "memory_list"},
		true,
	)
}

func (p ToolPolicy) AllowsBuiltin(name string) bool {
	_, ok := p.localTools[name]
	return ok
}

func (p ToolPolicy) AllowsMemory(name string) bool {
	_, ok := p.memoryTools[name]
	return ok
}

func (p ToolPolicy) BuiltinNames() []string {
	names := make([]string, 0, len(p.localTools))
	for name := range p.localTools {
		names = append(names, name)
	}
	return names
}

func (p ToolPolicy) MemoryNames() []string {
	names := make([]string, 0, len(p.memoryTools))
	for name := range p.memoryTools {
		names = append(names, name)
	}
	return names
}
