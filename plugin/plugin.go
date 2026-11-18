// Package maven 是 Lemon Paperplane 的 Maven 插件：识别 Maven reactor、选 JDK、临时 settings.xml 注入镜像、
// 同步有效 POM、编译并导出运行类路径（prepareClasspath 服务）、maven-goal 运行配置。
package maven

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const typeGoal = "maven-goal"

// Plugin 实现 maven 插件。
type Plugin struct{}

// New 返回插件实例。
func New() any { return &Plugin{} }

func (p *Plugin) cacheDir(ctx *sdk.Context) string {
	if ctx.Init != nil && ctx.Init.CacheDir != "" {
		return ctx.Init.CacheDir
	}
	return filepath.Join(os.TempDir(), "paperplane-maven")
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Detect 识别 pom.xml，认领 <modules> 里的子模块目录。
func (p *Plugin) Detect(ctx *sdk.Context, d *sdk.DetectParams) (*sdk.DetectResult, error) {
	if !sdk.HasFile(d.Files, "pom.xml") {
		return &sdk.DetectResult{}, nil
	}
	m := buildModel(d.Dir)
	if len(m.Projects) == 0 {
		return &sdk.DetectResult{}, nil
	}
	claims := []string{"src", "target", ".mvn"}
	var projects []map[string]any
	for _, pr := range m.Projects {
		projects = append(projects, map[string]any{"path": pr.Path, "dir": pr.RelDir})
		if pr.RelDir != "" && !strings.HasPrefix(pr.RelDir, "../") {
			claims = append(claims, pr.RelDir)
		}
	}
	data := map[string]any{"rootName": m.RootName, "projects": projects, "wrapper": m.Wrapper}
	return &sdk.DetectResult{Facets: []sdk.Facet{{ID: "maven", Data: data}}, Claims: claims, Name: m.Projects[0].Name}, nil
}

// Analyze 静态解析 POM；已有同步结果且 POM 没变时保留同步数据。
func (p *Plugin) Analyze(ctx *sdk.Context, a *sdk.AnalyzeParams) (*sdk.AnalyzeResult, error) {
	m := buildModel(a.Module.Dir)
	needsSync := m.ParentMissing
	var prev Model
	if a.Module.Model("maven", &prev) && prev.Synced {
		if prev.Fingerprint == m.Fingerprint {
			mergeSynced(m, &prev)
			needsSync = false
		} else {
			needsSync = true
		}
	}
	res := p.result(ctx, a, m)
	res.NeedsSync = needsSync
	return res, nil
}

func mavenVersion(root string, m *Model, tcs map[string]sdk.Toolchain) string {
	if useWrapper(root) && m.Version != "" {
		return m.Version
	}
	if tc, ok := tcs["maven"]; ok {
		return tc.Version
	}
	return ""
}

func (p *Plugin) result(ctx *sdk.Context, a *sdk.AnalyzeParams, m *Model) *sdk.AnalyzeResult {
	res := &sdk.AnalyzeResult{Model: mustJSON(m)}
	ver := mavenVersion(a.Module.Dir, m, a.Toolchains)
	res.Requirements = m.requirements(ver)
	if !useWrapper(a.Module.Dir) {
		res.Requirements = append(res.Requirements, sdk.ToolchainRequirement{Kind: "maven", Constraint: ">=3.6", Source: "pom.xml", Purpose: "build"})
	}
	if ver != "" {
		res.Info = append(res.Info, sdk.InfoItem{Label: "Maven", Value: ver})
	}
	if m.JavaTarget != "" {
		res.Info = append(res.Info, sdk.InfoItem{Label: "Java", Value: m.JavaTarget})
	}
	if n := javaModules(m); len(m.Projects) > 1 && n > 0 {
		res.Info = append(res.Info, sdk.InfoItem{Label: ctx.T("info.modules"), Value: itoa(n)})
	}
	if m.ParentMissing && !m.Synced {
		res.Problems = append(res.Problems, sdk.Diagnosis{Severity: "info", Title: ctx.T("problem.parent.title"), Message: ctx.T("problem.parent.message")})
	}
	opts := []sdk.Option{} // 没有选项也输出 []，不能是 null（界面按数组解析）
	for _, pr := range m.Projects {
		if pr.Java {
			opts = append(opts, sdk.Option{Value: pr.Path, Label: pr.Name})
		}
	}
	profiles := []sdk.Option{} // 没有选项也输出 []，不能是 null（界面按数组解析）
	for _, pf := range m.Profiles {
		profiles = append(profiles, sdk.Option{Value: pf})
	}
	res.OptionValues = map[string][]sdk.Option{"jvm.modules": opts, "maven.profiles": profiles}
	if m.Synced {
		res.Dependencies = &sdk.DependencyState{State: "ok", Manager: "maven"}
	}
	return res
}

// ---------------------------------------------------------------------------
// 同步：help:effective-pom 得到有效 POM（属性、父 POM、插件配置都已合并），go-offline 预先下载依赖与插件

// Sync 运行有效 POM 导出与依赖解析，合并进模型。
func (p *Plugin) Sync(ctx *sdk.Context, a *sdk.AnalyzeParams) (*sdk.AnalyzeResult, error) {
	root := a.Module.Dir
	m := buildModel(root)
	out := filepath.Join(p.cacheDir(ctx), "effective", sdk.StableID(root)+".xml")
	_ = os.Remove(out)
	ep, err := p.execParams(ctx, &invocation{
		Root: root, Model: m, Toolchains: a.Toolchains, Env: a.Env, Console: a.TaskID, Title: ctx.T("title.sync"),
		// go-offline 自己解析依赖并跳过 reactor 内的兄弟模块（resolve 会因兄弟模块尚未构建而失败）
		Goals: []string{helpPlugin + ":effective-pom", dependencyPlugin + ":go-offline"},
		Args:  []string{"-Doutput=" + out, "-DexcludeReactor=true", "-fae"},
	})
	if err != nil {
		return nil, err
	}
	ctx.Host.Progress(a.TaskID, ctx.T("title.sync"), -1)
	r, err := ctx.Host.Exec(ctx, ep)
	if err != nil {
		return nil, err
	}
	if r.Cancelled {
		return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	eff, perr := parseEffective(root, out)
	if perr != nil {
		return nil, sdk.UserError("%s", ctx.T("err.syncFailed"))
	}
	synced := &Model{Synced: true, SyncedAt: time.Now().Format(time.RFC3339), Projects: eff}
	mergeSynced(m, synced)
	res := p.result(ctx, a, m)
	if r.ExitCode != 0 {
		// 有效 POM 拿到了但依赖没解析全：模型照常更新，同时给出提示
		res.Problems = append(res.Problems, sdk.Diagnosis{Severity: "warning", Title: ctx.T("problem.resolve.title"), Message: ctx.T("problem.resolve.message")})
		res.Dependencies = &sdk.DependencyState{State: "missing", Manager: "maven", Message: ctx.T("problem.resolve.title")}
	}
	return res, nil
}

// parseEffective 解析 help:effective-pom 的输出（单模块是 <project>，多模块是 <projects>）。
func parseEffective(root, file string) ([]Project, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	text := string(data)
	var chunks []string
	for {
		start := strings.Index(text, "<project")
		if start < 0 {
			break
		}
		// 跳过 <projects>
		if strings.HasPrefix(text[start:], "<projects") {
			text = text[start+len("<projects"):]
			continue
		}
		end := strings.Index(text[start:], "</project>")
		if end < 0 {
			break
		}
		chunks = append(chunks, text[start:start+end+len("</project>")])
		text = text[start+end+len("</project>"):]
	}
	var out []Project
	for _, c := range chunks {
		tmp := filepath.Join(os.TempDir(), "pp-eff-"+sdk.StableID(c)+".xml")
		if err := os.WriteFile(tmp, []byte(c), 0o644); err != nil {
			continue
		}
		pf, err := readPom(tmp)
		os.Remove(tmp)
		if err != nil {
			continue
		}
		dir := ""
		if pf.Build.Directory != "" {
			dir = filepath.Dir(pf.Build.Directory)
		}
		if dir == "" {
			continue
		}
		props := map[string]string{}
		for k, v := range pf.Properties {
			props[k] = v
		}
		props["project.basedir"] = dir
		r := &resolved{pom: pf, dir: dir, props: props}
		pf.Parent = nil
		out = append(out, describe(root, r))
	}
	if len(out) == 0 {
		return nil, os.ErrNotExist
	}
	return out, nil
}

// mergeSynced 用有效 POM 的结果覆盖静态解析（按 artifactId 对应）。
func mergeSynced(m, s *Model) {
	m.Synced = true
	m.SyncedAt = s.SyncedAt
	for _, sp := range s.Projects {
		dst := m.project(sp.Path)
		if dst == nil {
			continue
		}
		if sp.JavaTarget != "" {
			dst.JavaTarget = sp.JavaTarget
		}
		dst.SpringBoot = dst.SpringBoot || sp.SpringBoot
		dst.SpringBootPlugin = dst.SpringBootPlugin || sp.SpringBootPlugin
		if sp.SpringBootVersion != "" {
			dst.SpringBootVersion = sp.SpringBootVersion
		}
		if sp.SpringBootMainClass != "" {
			dst.SpringBootMainClass = sp.SpringBootMainClass
		}
		if sp.MainClass != "" {
			dst.MainClass = sp.MainClass
		}
		dst.Kotlin = dst.Kotlin || sp.Kotlin
		if sp.KotlinVersion != "" {
			dst.KotlinVersion = sp.KotlinVersion
		}
		if len(sp.Dependencies) > 0 {
			dst.Dependencies = sp.Dependencies
		}
		if sp.OutputDir != "" {
			dst.OutputDir = sp.OutputDir
		}
	}
	var targets []string
	for _, pr := range m.Projects {
		targets = append(targets, pr.JavaTarget)
	}
	m.JavaTarget = maxJava(targets...)
}

// ---------------------------------------------------------------------------
// 服务：prepareClasspath

// ClasspathRequest 是 prepareClasspath 的输入。
type ClasspathRequest struct {
	Project    sdk.ProjectInfo          `json:"project"`
	Module     sdk.ModuleInfo           `json:"module"`
	Toolchains map[string]sdk.Toolchain `json:"toolchains,omitempty"`
	Env        map[string]string        `json:"env,omitempty"`
	RunID      string                   `json:"runId,omitempty"`
	Subproject string                   `json:"subproject,omitempty"` // artifactId / 相对目录，空为根模块
	Scope      string                   `json:"scope,omitempty"`      // runtime（默认）
}

// ClasspathResult 是 prepareClasspath 的输出。
type ClasspathResult struct {
	Classpath  []string `json:"classpath"`
	MainClass  string   `json:"mainClass,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
}

// CallService 处理 prepareClasspath。
func (p *Plugin) CallService(ctx *sdk.Context, method string, params json.RawMessage) (any, error) {
	if method == "launchGoals" {
		var req LaunchRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, sdk.NewError(sdk.ErrCodeInvalidParams, "%v", err)
		}
		return p.launchGoals(ctx, &req)
	}
	if method != "prepareClasspath" {
		return nil, sdk.ErrNotSupported
	}
	var req ClasspathRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, sdk.NewError(sdk.ErrCodeInvalidParams, "%v", err)
	}
	return p.prepareClasspath(ctx, &req)
}

// prepareClasspath：mvn -pl <模块> -am compile dependency:build-classpath。reactor 里的兄弟模块在 compile 阶段解析为它们的 target/classes。
func (p *Plugin) prepareClasspath(ctx *sdk.Context, req *ClasspathRequest) (*ClasspathResult, error) {
	root := req.Module.Dir
	m := buildModel(root)
	var prev Model
	if req.Module.Model("maven", &prev) && prev.Synced && prev.Fingerprint == m.Fingerprint {
		mergeSynced(m, &prev)
	}
	target := &m.Projects[0]
	if req.Subproject != "" {
		if t := m.project(req.Subproject); t != nil {
			target = t
		} else {
			return nil, sdk.UserError("%s", ctx.T("err.noModule", req.Subproject))
		}
	}
	scope := req.Scope
	if scope == "" {
		scope = "runtime"
	}
	outDir := filepath.Join(p.cacheDir(ctx), "classpath", sdk.StableID(root))
	_ = os.RemoveAll(outDir)
	_ = os.MkdirAll(outDir, 0o755)
	// 每个模块各写一个文件，避免 reactor 里多个模块写同一个文件
	args := []string{"-Dmdep.outputFile=" + filepath.Join(outDir, "${project.artifactId}.classpath"), "-Dmdep.includeScope=" + scope, "-DskipTests", "-Dmaven.test.skip.exec=true"}
	if len(m.Projects) > 1 {
		pl := target.RelDir
		if pl == "" {
			pl = "."
		}
		args = append(args, "-pl", pl, "-am")
	}
	ep, err := p.execParams(ctx, &invocation{
		Root: root, Model: m, Toolchains: req.Toolchains, Env: req.Env, Console: req.RunID, Title: ctx.T("title.compile"),
		Goals: []string{"compile", dependencyPlugin + ":build-classpath"}, Args: args,
	})
	if err != nil {
		return nil, err
	}
	r, err := ctx.Host.Exec(ctx, ep)
	if err != nil {
		return nil, err
	}
	if r.Cancelled {
		return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if r.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.buildFailed"))
	}
	file := filepath.Join(outDir, target.Path+".classpath")
	if !sdk.FileExists(file) {
		// 老版本 dependency 插件不展开 ${project.artifactId}：退回到字面文件名
		file = filepath.Join(outDir, "${project.artifactId}.classpath")
	}
	text := strings.TrimSpace(readText(file))
	if text == "" && !sdk.FileExists(file) {
		return nil, sdk.UserError("%s", ctx.T("err.noClasspath", target.Path))
	}
	cp := []string{target.OutputDir}
	if text != "" {
		cp = append(cp, strings.Split(text, string(os.PathListSeparator))...)
	}
	main := firstNonEmpty(target.SpringBootMainClass, target.MainClass)
	return &ClasspathResult{Classpath: cp, MainClass: main, WorkingDir: target.Dir}, nil
}

// LaunchRequest 是 launchGoals 服务的输入：把 Maven 目标作为运行主进程（如 spring-boot:run）。
type LaunchRequest struct {
	Project    sdk.ProjectInfo          `json:"project"`
	Module     sdk.ModuleInfo           `json:"module"`
	Toolchains map[string]sdk.Toolchain `json:"toolchains,omitempty"`
	Env        map[string]string        `json:"env,omitempty"`
	RunID      string                   `json:"runId,omitempty"`
	Goals      []string                 `json:"goals"`
	Args       []string                 `json:"args,omitempty"`
}

func (p *Plugin) launchGoals(ctx *sdk.Context, req *LaunchRequest) (*sdk.LaunchSpec, error) {
	if len(req.Goals) == 0 {
		return nil, sdk.UserError("%s", ctx.T("err.noGoals"))
	}
	m := buildModel(req.Module.Dir)
	ep, err := p.execParams(ctx, &invocation{Root: req.Module.Dir, Model: m, Toolchains: req.Toolchains, Env: req.Env, Console: req.RunID, Goals: req.Goals, Args: req.Args})
	if err != nil {
		return nil, err
	}
	return &sdk.LaunchSpec{Command: ep.Command, Args: ep.Args, Cwd: ep.Cwd, Env: ep.Env, PathPrepend: ep.PathPrepend,
		Display: "mvn " + strings.Join(append(append([]string{}, req.Args...), req.Goals...), " ")}, nil
}

// ---------------------------------------------------------------------------
// maven-goal 运行配置与动作

// PrepareRun 把 maven-goal 运行配置转成 mvn 命令。
func (p *Plugin) PrepareRun(ctx *sdk.Context, rp *sdk.PrepareRunParams) (*sdk.LaunchSpec, error) {
	if rp.Config.Type != typeGoal {
		return nil, sdk.ErrNotSupported
	}
	goals := sdk.SplitArgs(sdk.OptString(rp.Config.Options, "goals", ""))
	if len(goals) == 0 {
		return nil, sdk.UserError("%s", ctx.T("err.noGoals"))
	}
	var args []string
	if pf := sdk.OptStrings(rp.Config.Options, "profiles"); len(pf) > 0 {
		args = append(args, "-P", strings.Join(pf, ","))
	}
	args = append(args, sdk.SplitArgs(sdk.OptString(rp.Config.Options, "args", ""))...)
	m := buildModel(rp.Module.Dir)
	ep, err := p.execParams(ctx, &invocation{Root: rp.Module.Dir, Model: m, Toolchains: rp.Toolchains, Env: rp.Env, Console: rp.RunID, Goals: goals, Args: args})
	if err != nil {
		return nil, err
	}
	return &sdk.LaunchSpec{Command: ep.Command, Args: ep.Args, Cwd: ep.Cwd, Env: ep.Env, PathPrepend: ep.PathPrepend, Display: "mvn " + strings.Join(append(args, goals...), " ")}, nil
}

// InvokeAction 执行同步 / clean / package / install。
func (p *Plugin) InvokeAction(ctx *sdk.Context, a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	var goals, args []string
	switch a.Action {
	case "sync":
		// 内核在同一任务里执行 module/sync
		return &sdk.InvokeActionResult{Sync: true}, nil
	case "clean":
		goals = []string{"clean"}
	case "package":
		goals, args = []string{"package"}, []string{"-DskipTests"}
	case "install":
		goals, args = []string{"install"}, []string{"-DskipTests"}
	default:
		return nil, sdk.ErrNotSupported
	}
	m := buildModel(a.Module.Dir)
	ep, err := p.execParams(ctx, &invocation{Root: a.Module.Dir, Model: m, Toolchains: a.Toolchains, Env: a.Env, Console: a.TaskID, Goals: goals, Args: args})
	if err != nil {
		return nil, err
	}
	r, err := ctx.Host.Exec(ctx, ep)
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.goalFailed", strings.Join(goals, " ")))
	}
	return &sdk.InvokeActionResult{Message: ctx.T("msg.done")}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

func javaModules(m *Model) int {
	n := 0
	for _, p := range m.Projects {
		if p.Java {
			n++
		}
	}
	return n
}
