package maven

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 工具链 kind "maven"：工程没有 Maven Wrapper 时使用。

var mavenCoreRe = regexp.MustCompile(`^maven-core-(\d[\w.\-]*)\.jar$`)

// mavenHomeVersion 从 lib/maven-core-<v>.jar 读版本。
func mavenHomeVersion(home string) string {
	entries, _ := os.ReadDir(filepath.Join(home, "lib"))
	for _, e := range entries {
		if m := mavenCoreRe.FindStringSubmatch(e.Name()); m != nil {
			return m[1]
		}
	}
	return ""
}

func mavenToolchain(home, source string) *sdk.Toolchain {
	exe := filepath.Join(home, "bin", "mvn")
	if runtime.GOOS == "windows" {
		exe += ".cmd"
	}
	if !sdk.FileExists(exe) {
		return nil
	}
	v := mavenHomeVersion(home)
	if v == "" {
		return nil
	}
	return &sdk.Toolchain{
		Kind: "maven", Version: v, Vendor: "Apache", Path: home, Executable: exe,
		Env: map[string]string{"MAVEN_HOME": home}, PathPrepend: []string{filepath.Join(home, "bin")},
		Source: source, Managed: source == "managed", Label: "Maven " + v,
	}
}

func subDirs(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func discoverMaven(init *sdk.InitializeParams, paths []string) []sdk.Toolchain {
	type cand struct{ dir, source string }
	var cands []cand
	if len(paths) > 0 {
		for _, p := range paths {
			cands = append(cands, cand{p, "managed"})
			for _, s := range subDirs(p) {
				cands = append(cands, cand{s, "managed"})
			}
		}
	} else {
		if init != nil && init.HostDataDir != "" {
			for _, d := range subDirs(filepath.Join(init.HostDataDir, "toolchains", "maven")) {
				cands = append(cands, cand{d, "managed"})
			}
		}
		for _, d := range []string{"/opt/homebrew/opt/maven/libexec", "/usr/local/opt/maven/libexec", "/home/linuxbrew/.linuxbrew/opt/maven/libexec"} {
			cands = append(cands, cand{d, "homebrew"})
		}
		// Maven Wrapper 下好的发行包：~/.m2/wrapper/dists/<name>/<hash>/[apache-maven-x/]
		for _, d := range subDirs(filepath.Join(sdk.HomeDir(), ".m2", "wrapper", "dists")) {
			for _, h := range subDirs(d) {
				cands = append(cands, cand{h, "wrapper"})
				for _, x := range subDirs(h) {
					cands = append(cands, cand{x, "wrapper"})
				}
			}
		}
		for _, d := range subDirs(filepath.Join(sdk.HomeDir(), ".sdkman", "candidates", "maven")) {
			cands = append(cands, cand{d, "sdkman"})
		}
		for _, env := range []string{"MAVEN_HOME", "M2_HOME"} {
			if h := os.Getenv(env); h != "" {
				cands = append(cands, cand{h, "system"})
			}
		}
		if p, err := exec.LookPath(sdk.ExeName("mvn")); err == nil {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				cands = append(cands, cand{filepath.Dir(filepath.Dir(real)), "system"})
			}
		}
	}
	seen := map[string]bool{}
	seenVer := map[string]bool{}
	var out []sdk.Toolchain
	for _, c := range cands {
		real, err := filepath.EvalSymlinks(c.dir)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		tc := mavenToolchain(c.dir, c.source)
		if tc == nil || seenVer[tc.Version] {
			continue
		}
		seenVer[tc.Version] = true
		out = append(out, *tc)
	}
	return out
}

var metaVersionRe = regexp.MustCompile(`<version>([^<]+)</version>`)

func httpGet(ctx context.Context, u string, timeout time.Duration) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LemonPaperplane")
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

const centralURL = "https://repo.maven.apache.org/maven2"

func (p *Plugin) centralURLs(ctx *sdk.Context) []string {
	var out []string
	if ctx.Init != nil && ctx.Init.Mirrors.MavenCentral != "" {
		out = append(out, strings.TrimRight(ctx.Init.Mirrors.MavenCentral, "/"))
	}
	return append(out, centralURL)
}

// availableMaven 读 Maven Central 上 apache-maven 的版本元数据（镜像优先），只列正式版。
func (p *Plugin) availableMaven(ctx *sdk.Context) []sdk.AvailableVersion {
	var versions []string
	for _, base := range p.centralURLs(ctx) {
		b, err := httpGet(ctx, base+"/org/apache/maven/apache-maven/maven-metadata.xml", 15*time.Second)
		if err != nil {
			continue
		}
		for _, m := range metaVersionRe.FindAllStringSubmatch(string(b), -1) {
			v := m[1]
			lv := strings.ToLower(v)
			if strings.Contains(lv, "alpha") || strings.Contains(lv, "beta") || strings.Contains(lv, "rc") || !strings.HasPrefix(v, "3.") && !strings.HasPrefix(v, "4.") {
				continue
			}
			if sdk.CompareVersions(v, "3.6.0") < 0 {
				continue
			}
			versions = append(versions, v)
		}
		if len(versions) > 0 {
			break
		}
	}
	if len(versions) == 0 {
		versions = []string{"3.9.11", "3.8.8"}
	}
	sdk.SortVersionsDesc(versions)
	var out []sdk.AvailableVersion
	rec := false
	for _, v := range versions {
		r := !rec && strings.HasPrefix(v, "3.9.")
		if r {
			rec = true
		}
		out = append(out, sdk.AvailableVersion{Version: v, Label: "Maven " + v, Vendor: "Apache", Recommended: r})
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func (p *Plugin) installMavenPlan(ctx *sdk.Context, version string) (*sdk.InstallPlan, error) {
	v := strings.TrimSpace(version)
	if v == "" || strings.ContainsAny(v, "<>=^~ |*") {
		var cands []string
		for _, a := range p.availableMaven(ctx) {
			cands = append(cands, a.Version)
		}
		i := sdk.BestVersion(cands, v, "3.9")
		if i < 0 {
			return nil, sdk.UserError("%s", ctx.T("err.noMavenVersion", version))
		}
		v = cands[i]
	}
	archive, ext := "tar.gz", "tar.gz"
	if runtime.GOOS == "windows" {
		archive, ext = "zip", "zip"
	}
	file := "apache-maven-" + v + "-bin." + ext
	var urls []string
	for _, base := range p.centralURLs(ctx) {
		urls = append(urls, base+"/org/apache/maven/apache-maven/"+v+"/"+file)
	}
	return &sdk.InstallPlan{Version: v, URLs: urls, Archive: archive, StripComponents: 1, FileName: file}, nil
}

// DiscoverToolchains 发现本机 Maven。
func (p *Plugin) DiscoverToolchains(ctx *sdk.Context, d *sdk.DiscoverToolchainsParams) (*sdk.DiscoverToolchainsResult, error) {
	if d.Kind != "maven" {
		return nil, sdk.ErrNotSupported
	}
	return &sdk.DiscoverToolchainsResult{Toolchains: discoverMaven(ctx.Init, d.Paths)}, nil
}

// AvailableToolchains 列出可下载的 Maven 版本。
func (p *Plugin) AvailableToolchains(ctx *sdk.Context, a *sdk.AvailableToolchainsParams) (*sdk.AvailableToolchainsResult, error) {
	if a.Kind != "maven" {
		return nil, sdk.ErrNotSupported
	}
	return &sdk.AvailableToolchainsResult{Versions: p.availableMaven(ctx)}, nil
}

// InstallToolchain 返回 Maven 下载计划。
func (p *Plugin) InstallToolchain(ctx *sdk.Context, in *sdk.InstallToolchainParams) (*sdk.InstallToolchainResult, error) {
	if in.Kind != "maven" {
		return nil, sdk.ErrNotSupported
	}
	plan, err := p.installMavenPlan(ctx, in.Version)
	if err != nil {
		return nil, err
	}
	return &sdk.InstallToolchainResult{Plan: plan}, nil
}
