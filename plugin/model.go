package maven

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// Project 是 reactor 里的一个模块。字段与 gradle 插件的 Project 对齐，供 kotlin / spring-boot 统一读取。
type Project struct {
	Path                string   `json:"path"` // artifactId
	Name                string   `json:"name"`
	GroupID             string   `json:"groupId,omitempty"`
	Dir                 string   `json:"dir"`    // 绝对路径
	RelDir              string   `json:"relDir"` // 相对模块根
	Packaging           string   `json:"packaging"`
	JavaTarget          string   `json:"javaTarget,omitempty"`
	MainClass           string   `json:"mainClass,omitempty"`
	SpringBootMainClass string   `json:"springBootMainClass,omitempty"`
	Java                bool     `json:"java,omitempty"`
	SpringBoot          bool     `json:"springBoot,omitempty"`
	SpringBootPlugin    bool     `json:"springBootPlugin,omitempty"` // 配了 spring-boot-maven-plugin，可用 spring-boot:run
	SpringBootVersion   string   `json:"springBootVersion,omitempty"`
	Kotlin              bool     `json:"kotlin,omitempty"`
	KotlinVersion       string   `json:"kotlinVersion,omitempty"`
	Dependencies        []string `json:"dependencies,omitempty"`
	SourceDirs          []string `json:"sourceDirs,omitempty"`
	ResourceDirs        []string `json:"resourceDirs,omitempty"`
	OutputDir           string   `json:"outputDir,omitempty"`
	Profiles            []string `json:"profiles,omitempty"`
}

// Model 是 maven 插件的模型。
type Model struct {
	Tool          string    `json:"tool"` // 固定 "maven"
	Version       string    `json:"version,omitempty"`
	Wrapper       bool      `json:"wrapper"`
	RootName      string    `json:"rootName"`
	JavaTarget    string    `json:"javaTarget,omitempty"`
	Lombok        string    `json:"lombok,omitempty"`
	ParentMissing bool      `json:"parentMissing,omitempty"`
	Projects      []Project `json:"projects"`
	Fingerprint   string    `json:"fingerprint,omitempty"`
	Synced        bool      `json:"synced,omitempty"`
	SyncedAt      string    `json:"syncedAt,omitempty"`
	Profiles      []string  `json:"profiles,omitempty"`
}

var wrapperVersionRe = regexp.MustCompile(`apache-maven-([0-9][^/]*?)(?:-bin)?\.(?:zip|tar\.gz)`)

// buildModel 解析 reactor：根 POM 与递归的 <modules>。
func buildModel(root string) *Model {
	m := &Model{Tool: "maven"}
	if text := readText(filepath.Join(root, ".mvn", "wrapper", "maven-wrapper.properties")); text != "" {
		for _, line := range strings.Split(text, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.TrimSpace(k) == "distributionUrl" {
				if mm := wrapperVersionRe.FindStringSubmatch(strings.ReplaceAll(v, `\:`, ":")); mm != nil {
					m.Version = mm[1]
				}
			}
		}
		m.Wrapper = sdk.FileExists(filepath.Join(root, "mvnw")) || sdk.FileExists(filepath.Join(root, "mvnw.cmd"))
	}
	seen := map[string]bool{}
	profiles := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		abs, _ := filepath.Abs(dir)
		if seen[abs] || depth > 6 {
			return
		}
		seen[abs] = true
		r, err := resolvePom(filepath.Join(dir, "pom.xml"))
		if err != nil {
			return
		}
		pr := describe(root, r)
		if depth == 0 {
			m.RootName = pr.Path
			m.ParentMissing = r.extArtifact != "" && !isBootParent(r.extArtifact)
		}
		for _, p := range pr.Profiles {
			profiles[p] = true
		}
		m.Projects = append(m.Projects, pr)
		for _, mod := range r.pom.allModules() {
			sub := filepath.Join(dir, filepath.FromSlash(mod))
			if strings.HasSuffix(mod, ".xml") {
				sub = filepath.Dir(sub)
			}
			walk(sub, depth+1)
		}
	}
	walk(root, 0)
	var targets []string
	for _, pr := range m.Projects {
		targets = append(targets, pr.JavaTarget)
		if l := depVersion(pr.Dependencies, "org.projectlombok", "lombok"); l != "" && (m.Lombok == "" || sdk.CompareVersions(l, m.Lombok) < 0) {
			m.Lombok = l
		}
	}
	m.JavaTarget = maxJava(targets...)
	for p := range profiles {
		m.Profiles = append(m.Profiles, p)
	}
	sortStrings(m.Profiles)
	m.Fingerprint = fingerprint(root, m)
	return m
}

func isBootParent(a string) bool {
	return a == "spring-boot-starter-parent" || a == "spring-boot-dependencies"
}

// describe 从解析后的 POM 推导模块特征。
func describe(root string, r *resolved) Project {
	p := r.pom
	pr := Project{Path: p.ArtifactID, Name: firstNonEmpty(r.expand(p.Name), p.ArtifactID), Dir: r.dir, Packaging: firstNonEmpty(p.Packaging, "jar")}
	pr.GroupID = r.expand(firstNonEmpty(p.GroupID, parentField(p, "group")))
	if rel, err := filepath.Rel(root, r.dir); err == nil {
		pr.RelDir = filepath.ToSlash(rel)
		if pr.RelDir == "." {
			pr.RelDir = ""
		}
	}
	for _, x := range p.Profiles {
		if x.ID != "" {
			pr.Profiles = append(pr.Profiles, x.ID)
		}
	}
	// Spring Boot：父 POM、BOM、依赖或插件
	bootVersion := ""
	if isBootParent(r.extArtifact) && r.extGroup == "org.springframework.boot" {
		bootVersion = r.expand(r.extVersion)
		pr.SpringBoot = true
	}
	for _, d := range r.depMgmt() {
		if d.GroupID == "org.springframework.boot" && d.ArtifactID == "spring-boot-dependencies" && bootVersion == "" {
			bootVersion = d.Version
		}
	}
	if v := r.props["spring-boot.version"]; v != "" && bootVersion == "" {
		bootVersion = r.expand(v)
	}
	deps := map[string]bool{}
	for _, d := range r.allDeps() {
		if d.Scope == "test" {
			continue
		}
		c := d.GroupID + ":" + d.ArtifactID
		if d.Version != "" && !strings.Contains(d.Version, "${") {
			c += ":" + d.Version
		}
		if !deps[c] && interestingGroup(d.GroupID) {
			deps[c] = true
			pr.Dependencies = append(pr.Dependencies, c)
		}
		if d.GroupID == "org.springframework.boot" {
			pr.SpringBoot = true
			if bootVersion == "" && d.Version != "" && !strings.Contains(d.Version, "${") {
				bootVersion = d.Version
			}
		}
		if d.GroupID == "org.jetbrains.kotlin" && strings.HasPrefix(d.ArtifactID, "kotlin-stdlib") {
			pr.Kotlin = true
		}
	}
	if bp := r.usesPlugin("spring-boot-maven-plugin"); bp != nil {
		pr.SpringBoot = true
		pr.SpringBootPlugin = true
		pr.SpringBootMainClass = r.configValue(bp, "mainClass")
		if bootVersion == "" {
			bootVersion = r.expand(bp.Version)
		}
	}
	if pr.SpringBoot {
		pr.SpringBootVersion = bootVersion
	}
	if sc := r.props["start-class"]; sc != "" && pr.SpringBootMainClass == "" && !strings.Contains(r.expand(sc), "${") {
		pr.SpringBootMainClass = r.expand(sc)
	}
	for _, pl := range r.plugins() {
		switch pl.ArtifactID {
		case "exec-maven-plugin", "maven-jar-plugin", "maven-shade-plugin", "maven-assembly-plugin":
			if mc := r.configValue(&pl, "mainClass"); mc != "" && pr.MainClass == "" && !strings.Contains(mc, "${") {
				pr.MainClass = mc
			}
		case "kotlin-maven-plugin":
			if r.usesPlugin("kotlin-maven-plugin") != nil {
				pr.Kotlin = true
			}
		}
	}
	if pr.Kotlin {
		pr.KotlinVersion = firstNonEmpty(r.expand(r.props["kotlin.version"]), pluginVersion(r, "kotlin-maven-plugin"))
		if strings.Contains(pr.KotlinVersion, "${") {
			pr.KotlinVersion = ""
		}
	}
	// Java 版本
	target := ""
	for _, k := range []string{"maven.compiler.release", "maven.compiler.target", "maven.compiler.source"} {
		if v := normJava(r.expand(r.props[k])); v != "" {
			target = v
			break
		}
	}
	if target == "" {
		for _, pl := range r.plugins() {
			if pl.ArtifactID == "maven-compiler-plugin" {
				for _, k := range []string{"release", "target", "source"} {
					if v := normJava(r.configValue(&pl, k)); v != "" {
						target = v
						break
					}
				}
			}
			if target != "" {
				break
			}
		}
	}
	if target == "" {
		target = normJava(r.expand(r.props["java.version"]))
	}
	if target == "" && pr.SpringBoot && isBootParent(r.extArtifact) {
		// spring-boot-starter-parent 的默认 java.version
		if strings.HasPrefix(bootVersion, "2.") || strings.HasPrefix(bootVersion, "1.") {
			target = "8"
		} else if bootVersion != "" {
			target = "17"
		}
	}
	pr.JavaTarget = target
	// 目录
	pr.Java = pr.Packaging == "jar" || pr.Packaging == "war" || pr.Packaging == "bundle" || pr.Packaging == "maven-plugin"
	srcDir := filepath.Join(r.dir, "src", "main", "java")
	if sd := r.expand(p.Build.SourceDirectory); sd != "" && !strings.Contains(sd, "${") {
		if !filepath.IsAbs(sd) {
			sd = filepath.Join(r.dir, sd)
		}
		srcDir = sd
	}
	for _, d := range []string{srcDir, filepath.Join(r.dir, "src", "main", "kotlin")} {
		if sdk.DirExists(d) {
			pr.SourceDirs = append(pr.SourceDirs, d)
			if strings.HasSuffix(d, "kotlin") {
				pr.Kotlin = true
			}
		}
	}
	if d := filepath.Join(r.dir, "src", "main", "resources"); sdk.DirExists(d) {
		pr.ResourceDirs = append(pr.ResourceDirs, d)
	}
	pr.OutputDir = filepath.Join(r.dir, "target", "classes")
	if od := r.expand(p.Build.OutputDirectory); od != "" && !strings.Contains(od, "${") {
		if !filepath.IsAbs(od) {
			od = filepath.Join(r.dir, od)
		}
		pr.OutputDir = od
	}
	if pr.Packaging == "pom" {
		pr.Java = false
	}
	return pr
}

func pluginVersion(r *resolved, artifact string) string {
	for _, pl := range r.plugins() {
		if pl.ArtifactID == artifact && pl.Version != "" {
			return r.expand(pl.Version)
		}
	}
	return ""
}

func interestingGroup(g string) bool {
	for _, p := range []string{"org.springframework", "com.alibaba", "org.jetbrains.kotlin", "com.oracle.database", "org.mybatis"} {
		if strings.HasPrefix(g, p) {
			return true
		}
	}
	switch g {
	case "com.mysql", "mysql", "org.postgresql", "com.h2database", "org.mariadb.jdbc", "com.microsoft.sqlserver", "redis.clients", "io.lettuce",
		"org.apache.kafka", "com.rabbitmq", "co.elastic.clients", "org.elasticsearch.client", "org.mongodb", "org.projectlombok",
		"io.micrometer", "org.flywaydb", "org.liquibase", "com.baomidou", "org.redisson":
		return true
	}
	return false
}

func depVersion(deps []string, group, name string) string {
	for _, d := range deps {
		parts := strings.Split(d, ":")
		if len(parts) >= 3 && parts[0] == group && parts[1] == name {
			return parts[2]
		}
	}
	return ""
}

func maxJava(vs ...string) string {
	best := 0
	for _, v := range vs {
		if n, err := strconv.Atoi(v); err == nil && n > best {
			best = n
		}
	}
	if best == 0 {
		return ""
	}
	return strconv.Itoa(best)
}

func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// fingerprint 汇总所有 POM 与 Maven 配置文件的路径、大小、修改时间。
func fingerprint(root string, m *Model) string {
	files := []string{filepath.Join(root, ".mvn", "maven.config"), filepath.Join(root, ".mvn", "wrapper", "maven-wrapper.properties")}
	for _, p := range m.Projects {
		files = append(files, filepath.Join(p.Dir, "pom.xml"))
	}
	var parts []string
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			parts = append(parts, fmt.Sprintf("%s|%d|%d", f, st.Size(), st.ModTime().UnixNano()))
		}
	}
	return sdk.StableID(parts...)
}

// project 按 artifactId / 相对目录 / 名称找模块。
func (m *Model) project(v string) *Project {
	v = strings.TrimPrefix(v, ":")
	for i := range m.Projects {
		p := &m.Projects[i]
		if p.Path == v || p.RelDir == filepath.ToSlash(v) || p.Dir == v || p.GroupID+":"+p.Path == v {
			return p
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// JDK 需求

// mavenMinJava：运行 Maven 所需的最低 Java。
func mavenMinJava(version string) int {
	switch {
	case version == "":
		return 8
	case sdk.CompareVersions(version, "4.0.0-alpha") >= 0:
		return 17
	case sdk.CompareVersions(version, "3.9.0") >= 0:
		return 8
	case sdk.CompareVersions(version, "3.3.0") >= 0:
		return 7
	}
	return 6
}

// oldTargetMaxJava：老的源码/目标版本在新 JDK 上已不受支持。
func oldTargetMaxJava(target int) int {
	switch {
	case target > 0 && target <= 5:
		return 8
	case target == 6:
		return 11
	case target == 7:
		return 19
	}
	return 0
}

func lombokMaxJava(version string) int {
	switch {
	case version == "":
		return 0
	case sdk.CompareVersions(version, "1.18.20") < 0:
		return 15
	case sdk.CompareVersions(version, "1.18.22") < 0:
		return 16
	case sdk.CompareVersions(version, "1.18.30") < 0:
		return 17
	}
	return 0
}

func (m *Model) requirements(mavenVersion string) []sdk.ToolchainRequirement {
	lo := mavenMinJava(mavenVersion)
	target, _ := strconv.Atoi(m.JavaTarget)
	if target > lo {
		lo = target
	}
	c := fmt.Sprintf(">=%d", lo)
	if hi := oldTargetMaxJava(target); hi > 0 {
		c = fmt.Sprintf(">=%d <%d", mavenMinJava(mavenVersion), hi+1)
	}
	src := "Maven"
	if mavenVersion != "" {
		src += " " + mavenVersion
	}
	if target > 0 {
		src += " · Java " + m.JavaTarget
	}
	req := sdk.ToolchainRequirement{Kind: "jdk", Constraint: c, Source: src, Purpose: "maven-jvm"}
	if target > 0 {
		req.Preferred = m.JavaTarget
	}
	reqs := []sdk.ToolchainRequirement{req}
	if mx := lombokMaxJava(m.Lombok); mx > 0 {
		reqs = append(reqs, sdk.ToolchainRequirement{Kind: "jdk", Constraint: fmt.Sprintf("<%d", mx+1), Source: "Lombok " + m.Lombok, Purpose: "compile"})
	}
	return reqs
}
