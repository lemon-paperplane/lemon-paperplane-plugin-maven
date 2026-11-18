package maven

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// pomFile 是 pom.xml 里本插件关心的部分。
type pomFile struct {
	Parent *struct {
		GroupID      string  `xml:"groupId"`
		ArtifactID   string  `xml:"artifactId"`
		Version      string  `xml:"version"`
		RelativePath *string `xml:"relativePath"`
	} `xml:"parent"`
	GroupID      string     `xml:"groupId"`
	ArtifactID   string     `xml:"artifactId"`
	Version      string     `xml:"version"`
	Packaging    string     `xml:"packaging"`
	Name         string     `xml:"name"`
	Properties   properties `xml:"properties"`
	Modules      []string   `xml:"modules>module"`
	Profiles     []profile  `xml:"profiles>profile"`
	Dependencies []dep      `xml:"dependencies>dependency"`
	DepMgmt      []dep      `xml:"dependencyManagement>dependencies>dependency"`
	Build        struct {
		Directory       string   `xml:"directory"`
		SourceDirectory string   `xml:"sourceDirectory"`
		OutputDirectory string   `xml:"outputDirectory"`
		Plugins         []plugin `xml:"plugins>plugin"`
		PluginMgmt      []plugin `xml:"pluginManagement>plugins>plugin"`
	} `xml:"build"`
}

type profile struct {
	ID         string     `xml:"id"`
	Modules    []string   `xml:"modules>module"`
	Properties properties `xml:"properties"`
}

type dep struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
	Type       string `xml:"type"`
}

type plugin struct {
	GroupID       string `xml:"groupId"`
	ArtifactID    string `xml:"artifactId"`
	Version       string `xml:"version"`
	Configuration struct {
		Inner string `xml:",innerxml"`
	} `xml:"configuration"`
	Executions []struct {
		Configuration struct {
			Inner string `xml:",innerxml"`
		} `xml:"configuration"`
	} `xml:"executions>execution"`
}

// properties 把 <properties> 下的子元素读成 map。
type properties map[string]string

func (p *properties) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	m := map[string]string{}
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var v string
			if err := d.DecodeElement(&v, &t); err != nil {
				return err
			}
			m[t.Name.Local] = strings.TrimSpace(v)
		case xml.EndElement:
			*p = m
			return nil
		}
	}
}

func readPom(path string) (*pomFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p pomFile
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.CharsetReader = func(label string, input io.Reader) (io.Reader, error) { return input, nil }
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// allModules 返回 <modules> 与各 profile 里的模块（去重保序）。
func (p *pomFile) allModules() []string {
	seen := map[string]bool{}
	var out []string
	add := func(ms []string) {
		for _, m := range ms {
			m = strings.TrimSpace(m)
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	add(p.Modules)
	for _, pr := range p.Profiles {
		add(pr.Modules)
	}
	return out
}

// ---------------------------------------------------------------------------
// 属性展开

var propRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// resolved 是合并了父 POM 之后的 POM 视图。
type resolved struct {
	pom     *pomFile
	dir     string
	props   map[string]string
	parents []*pomFile // 由近及远的本地父 POM
	// 父 POM 坐标（可能是不在本地的 spring-boot-starter-parent）
	parentGroup, parentArtifact, parentVersion string
	parentMissing                              bool
	// 父 POM 链上第一个不在本地的父 POM（例如 spring-boot-starter-parent）
	extGroup, extArtifact, extVersion string
}

// resolvePom 读取 POM 并沿 relativePath 合并本地父 POM 的属性。
func resolvePom(path string) (*resolved, error) {
	p, err := readPom(path)
	if err != nil {
		return nil, err
	}
	r := &resolved{pom: p, dir: filepath.Dir(path), props: map[string]string{}}
	chain := []*pomFile{p}
	cur, curDir := p, r.dir
	for depth := 0; depth < 8 && cur.Parent != nil; depth++ {
		if depth == 0 {
			r.parentGroup, r.parentArtifact, r.parentVersion = cur.Parent.GroupID, cur.Parent.ArtifactID, cur.Parent.Version
		}
		rel := "../pom.xml"
		if cur.Parent.RelativePath != nil {
			rel = strings.TrimSpace(*cur.Parent.RelativePath)
		}
		if rel == "" {
			if depth == 0 {
				r.parentMissing = true
			}
			r.extGroup, r.extArtifact, r.extVersion = cur.Parent.GroupID, cur.Parent.ArtifactID, cur.Parent.Version
			break
		}
		pp := filepath.Join(curDir, rel)
		if st, err := os.Stat(pp); err == nil && st.IsDir() {
			pp = filepath.Join(pp, "pom.xml")
		}
		parent, err := readPom(pp)
		if err != nil || parent.ArtifactID != cur.Parent.ArtifactID {
			if depth == 0 {
				r.parentMissing = true
			}
			r.extGroup, r.extArtifact, r.extVersion = cur.Parent.GroupID, cur.Parent.ArtifactID, cur.Parent.Version
			break
		}
		r.parents = append(r.parents, parent)
		chain = append(chain, parent)
		cur, curDir = parent, filepath.Dir(pp)
	}
	// 远的先放，近的覆盖
	for i := len(chain) - 1; i >= 0; i-- {
		for k, v := range chain[i].Properties {
			r.props[k] = v
		}
	}
	group := firstNonEmpty(p.GroupID, parentField(p, "group"))
	version := firstNonEmpty(p.Version, parentField(p, "version"))
	for k, v := range map[string]string{
		"project.groupId": group, "pom.groupId": group, "groupId": group,
		"project.artifactId": p.ArtifactID, "pom.artifactId": p.ArtifactID,
		"project.version": version, "pom.version": version, "version": version,
		"project.parent.version": parentField(p, "version"), "parent.version": parentField(p, "version"),
		"project.basedir": r.dir, "basedir": r.dir,
	} {
		if v != "" {
			if _, ok := r.props[k]; !ok || strings.HasPrefix(k, "project.") || strings.HasPrefix(k, "pom.") || k == "basedir" {
				r.props[k] = v
			}
		}
	}
	return r, nil
}

func parentField(p *pomFile, f string) string {
	if p.Parent == nil {
		return ""
	}
	switch f {
	case "group":
		return p.Parent.GroupID
	case "version":
		return p.Parent.Version
	}
	return ""
}

// expand 展开 ${x}，最多 6 轮；展不开的保持原样。
func (r *resolved) expand(s string) string {
	for i := 0; i < 6 && strings.Contains(s, "${"); i++ {
		next := propRe.ReplaceAllStringFunc(s, func(m string) string {
			k := m[2 : len(m)-1]
			if v, ok := r.props[k]; ok {
				return v
			}
			return m
		})
		if next == s {
			break
		}
		s = next
	}
	return strings.TrimSpace(s)
}

// allDeps 返回本 POM 与本地父 POM 的依赖（含依赖管理），已展开属性。
func (r *resolved) allDeps() []dep {
	var out []dep
	add := func(ds []dep) {
		for _, d := range ds {
			d.GroupID, d.ArtifactID, d.Version = r.expand(d.GroupID), r.expand(d.ArtifactID), r.expand(d.Version)
			out = append(out, d)
		}
	}
	add(r.pom.Dependencies)
	for _, p := range r.parents {
		add(p.Dependencies)
	}
	return out
}

func (r *resolved) depMgmt() []dep {
	var out []dep
	for _, p := range append([]*pomFile{r.pom}, r.parents...) {
		for _, d := range p.DepMgmt {
			d.GroupID, d.ArtifactID, d.Version = r.expand(d.GroupID), r.expand(d.ArtifactID), r.expand(d.Version)
			out = append(out, d)
		}
	}
	return out
}

// plugins 返回本 POM 与本地父 POM 的插件（含 pluginManagement 里的配置）。
func (r *resolved) plugins() []plugin {
	var out []plugin
	for _, p := range append([]*pomFile{r.pom}, r.parents...) {
		out = append(out, p.Build.Plugins...)
		out = append(out, p.Build.PluginMgmt...)
	}
	return out
}

// usesPlugin 判断本 POM 的 <build><plugins> 里是否声明了某插件（不算 pluginManagement）。
func (r *resolved) usesPlugin(artifact string) *plugin {
	for _, p := range append([]*pomFile{r.pom}, r.parents...) {
		for i := range p.Build.Plugins {
			if p.Build.Plugins[i].ArtifactID == artifact {
				return &p.Build.Plugins[i]
			}
		}
	}
	return nil
}

// configValue 在插件配置（含 executions）里找某个元素的文本。
func (r *resolved) configValue(pl *plugin, name string) string {
	re := regexp.MustCompile(`<` + name + `>\s*([^<]+?)\s*</` + name + `>`)
	texts := []string{pl.Configuration.Inner}
	for _, e := range pl.Executions {
		texts = append(texts, e.Configuration.Inner)
	}
	for _, t := range texts {
		if m := re.FindStringSubmatch(t); m != nil {
			return r.expand(m[1])
		}
	}
	return ""
}

// normJava 把 "1.8" / "17" / "21-ea" 规范成主版本号。
func normJava(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "1.")
	if i := strings.IndexAny(v, ".-_"); i >= 0 {
		v = v[:i]
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 5 && n < 100 {
		return strconv.Itoa(n)
	}
	return ""
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
