package maven

import (
	"runtime"
	"strings"
	"testing"
)

func TestReactorModel(t *testing.T) {
	m := buildModel("testdata/reactor")
	if m.RootName != "acme-parent" || m.Version != "3.9.6" || !m.Wrapper {
		t.Errorf("root %q version %q wrapper %v", m.RootName, m.Version, m.Wrapper)
	}
	if len(m.Projects) != 4 {
		t.Fatalf("projects %d", len(m.Projects))
	}
	if m.Projects[0].Java || m.Projects[0].Packaging != "pom" {
		t.Errorf("aggregator %+v", m.Projects[0])
	}
	web := m.project("acme-web")
	if web == nil || !web.SpringBoot || !web.SpringBootPlugin || web.SpringBootVersion != "2.7.18" || web.SpringBootMainClass != "com.acme.web.WebApp" || web.JavaTarget != "11" || web.Packaging != "war" || web.RelDir != "web" {
		t.Errorf("web %+v", web)
	}
	if m.project("extra") == nil {
		t.Error("profile module missing")
	}
	if m.Lombok != "1.18.20" {
		t.Errorf("lombok %q", m.Lombok)
	}
	for _, d := range web.Dependencies {
		if strings.Contains(d, "junit") {
			t.Error("test dependency kept")
		}
	}
	reqs := m.requirements("3.9.6")
	if reqs[0].Constraint != ">=11" || reqs[0].Preferred != "11" || len(reqs) != 2 || reqs[1].Constraint != "<17" {
		t.Errorf("reqs %+v", reqs)
	}
	if len(m.Profiles) != 1 || m.Profiles[0] != "extra" {
		t.Errorf("profiles %v", m.Profiles)
	}
}

func TestBomAndKotlin(t *testing.T) {
	m := buildModel("testdata/bom")
	p := m.Projects[0]
	if !p.SpringBoot || p.SpringBootVersion != "3.3.4" || p.SpringBootMainClass != "com.acme.BomAppKt" || !p.Kotlin || p.KotlinVersion != "1.9.25" || p.JavaTarget != "21" {
		t.Errorf("bom %+v", p)
	}
}

func TestParseEffective(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("夹具里是 Unix 绝对路径")
	}
	ps, err := parseEffective("/work/acme", "testdata/effective.xml")
	if err != nil || len(ps) != 2 {
		t.Fatalf("%v %d", err, len(ps))
	}
	if ps[0].Path != "acme-core" || ps[0].JavaTarget != "11" || ps[0].Dir != "/work/acme/core" || ps[0].RelDir != "core" || ps[0].OutputDir != "/work/acme/core/target/classes" {
		t.Errorf("core %+v", ps[0])
	}
	if ps[1].JavaTarget != "17" || ps[1].SpringBootMainClass != "com.acme.web.WebApp" {
		t.Errorf("web %+v", ps[1])
	}
}

func TestSettingsXML(t *testing.T) {
	s := settingsXML("https://maven.aliyun.com/repository/public")
	if !strings.Contains(s, "<mirrorOf>central</mirrorOf>") || !strings.Contains(s, "maven-default-http-blocker") || !strings.Contains(s, "<id>central</id>") {
		t.Error(s)
	}
	if !onlyHTTPBlocker("<mirrors><mirror><id>maven-default-http-blocker</id></mirror></mirrors>") {
		t.Error("blocker only")
	}
	if hasMirror := mirrorTagRe.MatchString(stripXMLComments("<mirrors><!-- <mirror> --></mirrors>")); hasMirror {
		t.Error("commented mirror counted")
	}
}

func TestMavenMinJava(t *testing.T) {
	cases := map[string]int{"4.0.0": 17, "4.0.0-rc-4": 17, "3.9.6": 8, "3.8.8": 7, "": 8}
	for v, want := range cases {
		if got := mavenMinJava(v); got != want {
			t.Errorf("%s → %d want %d", v, got, want)
		}
	}
	m := &Model{JavaTarget: "6"}
	if r := m.requirements("3.9.6"); r[0].Constraint != ">=8 <12" {
		t.Errorf("old target %+v", r)
	}
}
