package maven

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 固定插件版本，避免每次按仓库元数据解析最新版
const (
	helpPlugin       = "org.apache.maven.plugins:maven-help-plugin:3.5.1"
	dependencyPlugin = "org.apache.maven.plugins:maven-dependency-plugin:3.8.1"
)

type invocation struct {
	Root       string
	Model      *Model
	Toolchains map[string]sdk.Toolchain
	Env        map[string]string
	Console    string
	Title      string
	Goals      []string
	Args       []string
}

func useWrapper(root string) bool {
	if !sdk.FileExists(filepath.Join(root, ".mvn", "wrapper", "maven-wrapper.properties")) {
		return false
	}
	if runtime.GOOS == "windows" {
		return sdk.FileExists(filepath.Join(root, "mvnw.cmd"))
	}
	return sdk.FileExists(filepath.Join(root, "mvnw"))
}

// command 选出 Maven 可执行文件：mvnw 优先，其次选定的 maven 工具链，最后 PATH 里的 mvn。
func command(ctx *sdk.Context, root string, tcs map[string]sdk.Toolchain) (string, []string, string, error) {
	if useWrapper(root) {
		if runtime.GOOS == "windows" {
			return filepath.Join(root, "mvnw.cmd"), nil, "", nil
		}
		w := filepath.Join(root, "mvnw")
		if st, err := os.Stat(w); err == nil && st.Mode()&0o111 != 0 {
			return w, nil, "", nil
		}
		return "/bin/sh", []string{w}, "", nil
	}
	if tc, ok := tcs["maven"]; ok && tc.Executable != "" {
		return tc.Executable, nil, tc.Path, nil
	}
	if p, err := exec.LookPath(sdk.ExeName("mvn")); err == nil {
		home := ""
		if real, err := filepath.EvalSymlinks(p); err == nil {
			home = filepath.Dir(filepath.Dir(real))
		}
		if runtime.GOOS == "windows" {
			if p2, err := exec.LookPath("mvn.cmd"); err == nil {
				p = p2
			}
		}
		return p, nil, home, nil
	}
	return "", nil, "", sdk.UserError("%s", ctx.T("err.noMaven"))
}

var mirrorTagRe = regexp.MustCompile(`(?s)<mirrors>.*?<mirror>`)

// hasCustomMirrors 判断用户（或 Maven 安装目录）的 settings.xml 是否已配置镜像；有则尊重用户配置，不再注入。
func hasCustomMirrors(mavenHome string) bool {
	files := []string{filepath.Join(sdk.HomeDir(), ".m2", "settings.xml")}
	if mavenHome != "" {
		files = append(files, filepath.Join(mavenHome, "conf", "settings.xml"))
	}
	for _, f := range files {
		text := stripXMLComments(readText(f))
		if mirrorTagRe.MatchString(text) && !onlyHTTPBlocker(text) {
			return true
		}
	}
	return false
}

var xmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

func stripXMLComments(s string) string { return xmlCommentRe.ReplaceAllString(s, "") }

// onlyHTTPBlocker：Maven 3.8.1+ 自带的 settings.xml 只有一个屏蔽 HTTP 仓库的镜像。
func onlyHTTPBlocker(text string) bool {
	return strings.Count(text, "<mirror>") == 1 && strings.Contains(text, "maven-default-http-blocker")
}

// settingsXML 生成临时全局 settings：把 central 指向镜像，并保留 Maven 默认的 HTTP 仓库屏蔽。
func settingsXML(mirror string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!-- Lemon Paperplane 生成的临时全局 settings（-gs），用户的 ~/.m2/settings.xml 仍然生效 -->
<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0">
  <mirrors>
    <mirror>
      <id>central</id>
      <mirrorOf>central</mirrorOf>
      <url>` + xmlEscape(mirror) + `</url>
    </mirror>
    <mirror>
      <id>maven-default-http-blocker</id>
      <mirrorOf>external:http:*</mirrorOf>
      <url>http://0.0.0.0/</url>
      <blocked>true</blocked>
    </mirror>
  </mirrors>
</settings>
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func (p *Plugin) mirrorArgs(ctx *sdk.Context, mavenHome string) []string {
	if ctx.Init == nil || ctx.Init.Mirrors.MavenCentral == "" || hasCustomMirrors(mavenHome) {
		return nil
	}
	mirror := ctx.Init.Mirrors.MavenCentral
	path := filepath.Join(p.cacheDir(ctx), "settings-"+sdk.StableID(mirror)+".xml")
	if !sdk.FileExists(path) {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(settingsXML(mirror)), 0o644); err != nil {
			return nil
		}
	}
	return []string{"-gs", path}
}

// execParams 组装一次 Maven 调用：JAVA_HOME 用选定 JDK，批处理模式，镜像经 -gs 注入。
func (p *Plugin) execParams(ctx *sdk.Context, in *invocation) (*sdk.ExecParams, error) {
	jdk, ok := in.Toolchains["jdk"]
	if !ok || jdk.Path == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noJdk"))
	}
	cmd, pre, home, err := command(ctx, in.Root, in.Toolchains)
	if err != nil {
		return nil, err
	}
	ver := in.Model.Version
	if !useWrapper(in.Root) {
		if tc, ok := in.Toolchains["maven"]; ok {
			ver = tc.Version
		} else if home != "" {
			ver = mavenHomeVersion(home)
		}
	}
	args := append([]string{}, pre...)
	args = append(args, "-B")
	if ver == "" || sdk.CompareVersions(ver, "3.6.1") >= 0 {
		args = append(args, "-ntp")
	}
	args = append(args, p.mirrorArgs(ctx, home)...)
	args = append(args, in.Args...)
	args = append(args, in.Goals...)
	env := sdk.MergeEnv(in.Env, jdk.Env, map[string]string{"JAVA_HOME": jdk.Path})
	if ctx.Init != nil && ctx.Init.Mirrors.MavenCentral != "" && useWrapper(in.Root) {
		// Maven Wrapper 3.x 用 MVNW_REPOURL 替换下载 Maven 发行包的仓库地址
		env["MVNW_REPOURL"] = strings.TrimRight(ctx.Init.Mirrors.MavenCentral, "/")
	}
	return &sdk.ExecParams{
		Command:     cmd,
		Args:        args,
		Cwd:         in.Root,
		Env:         env,
		PathPrepend: jdk.PathPrepend,
		Console:     in.Console,
		Title:       in.Title,
	}, nil
}
