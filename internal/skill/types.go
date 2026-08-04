package skill

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxSkillBodyBytes is the shared discovery and on-demand loading boundary.
const MaxSkillBodyBytes = 1 << 20

// Skill represents a loadable skill definition.
type Skill struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	Active       bool   `yaml:"-"`
	Content      string `yaml:"-"` // markdown body after frontmatter
	Path         string `yaml:"-"` // file path
	nameDeclared bool   // distinguishes an omitted name from an explicitly blank one
}

type optionalFrontmatterString struct {
	value string
	set   bool
}

func (value *optionalFrontmatterString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("must be a string")
	}
	value.value = node.Value
	value.set = true
	return nil
}

// skillFrontmatter is an explicit compatibility schema. sonar consumes
// only name and description, but accepts the remaining Agent Skills metadata
// fields without projecting them into prompts or session state. Typos and
// unsupported fields fail discovery instead of being silently ignored.
type skillFrontmatter struct {
	Name          optionalFrontmatterString `yaml:"name"`
	Description   optionalFrontmatterString `yaml:"description"`
	License       yaml.Node                 `yaml:"license"`
	Compatibility yaml.Node                 `yaml:"compatibility"`
	Metadata      yaml.Node                 `yaml:"metadata"`
	AllowedTools  yaml.Node                 `yaml:"allowed-tools"`
	Triggers      yaml.Node                 `yaml:"triggers"`
}

// CatalogEntry is the bounded, model-safe projection of a discovered skill.
// It deliberately contains neither the skill body nor its filesystem path.
type CatalogEntry struct {
	Name        string
	Description string
}

// parseFrontmatter extracts YAML frontmatter and markdown body from a skill file.
// Frontmatter is delimited by "---" on the first and closing lines.
func parseFrontmatter(data string) (*Skill, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	// Discovery bounds the file before parsing, but Scanner's default token
	// limit is smaller. A valid long Markdown line must not be truncated.
	scanner.Buffer(make([]byte, 64*1024), MaxSkillBodyBytes+1)

	// Check for opening "---".
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return &Skill{Content: data}, nil
	}
	if strings.TrimSpace(scanner.Text()) != "---" {
		// No frontmatter — treat entire content as body.
		return &Skill{Content: data}, nil
	}

	// Read YAML lines until closing "---".
	var yamlBuf strings.Builder
	foundEnd := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			foundEnd = true
			break
		}
		yamlBuf.WriteString(line)
		yamlBuf.WriteString("\n")
	}

	if !foundEnd {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		// No closing delimiter — treat as body only.
		return &Skill{Content: data}, nil
	}

	// Parse one strict YAML frontmatter document. Known Agent Skills extension
	// fields are accepted by skillFrontmatter, while unsupported keys fail.
	metadata := skillFrontmatter{}
	decoder := yaml.NewDecoder(strings.NewReader(yamlBuf.String()))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	s := &Skill{
		Name:         metadata.Name.value,
		Description:  metadata.Description.value,
		nameDeclared: metadata.Name.set,
	}

	// Remaining content is the markdown body.
	var bodyBuf strings.Builder
	for scanner.Scan() {
		if bodyBuf.Len() > 0 {
			bodyBuf.WriteString("\n")
		}
		bodyBuf.WriteString(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	s.Content = strings.TrimSpace(bodyBuf.String())

	return s, nil
}
