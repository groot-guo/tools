// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package modfile

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/gopls/internal/test/compare"
	. "golang.org/x/tools/gopls/internal/test/integration"
	"golang.org/x/tools/gopls/internal/util/bug"

	"golang.org/x/tools/gopls/internal/protocol"
)

func TestMain(m *testing.M) {
	bug.PanicOnBugs = true
	os.Exit(Main(m))
}

const workspaceProxy = `
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12
-- example.com@v1.2.3/blah/blah.go --
package blah

func SaySomething() {
	fmt.Println("something")
}
-- random.org@v1.2.3/go.mod --
module random.org

go 1.12
-- random.org@v1.2.3/bye/bye.go --
package bye

func Goodbye() {
	println("Bye")
}
`

const proxy = `
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12
-- example.com@v1.2.3/blah/blah.go --
package blah

const Name = "Blah"
-- random.org@v1.2.3/go.mod --
module random.org

go 1.12
-- random.org@v1.2.3/blah/blah.go --
package hello

const Name = "Hello"
`

func TestModFileModification(t *testing.T) {
	const untidyModule = `
-- a/go.mod --
module mod.com

-- a/main.go --
package main

import "example.com/blah"

func main() {
	println(blah.Name)
}
`

	runner := RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}

	t.Run("basic", func(t *testing.T) {
		runner.Run(t, untidyModule, func(t *testing.T, env *Env) {
			// Open the file and make sure that the initial workspace load does not
			// modify the go.mod file.
			goModContent := env.ReadWorkspaceFile("a/go.mod")
			env.OpenFile("a/main.go")
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/main.go", "\"example.com/blah\"")),
			)
			if got := env.ReadWorkspaceFile("a/go.mod"); got != goModContent {
				t.Fatalf("go.mod changed on disk:\n%s", compare.Text(goModContent, got))
			}
			// Save the buffer, which will format and organize imports.
			// Confirm that the go.mod file still does not change.
			env.SaveBuffer("a/main.go")
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/main.go", "\"example.com/blah\"")),
			)
			if got := env.ReadWorkspaceFile("a/go.mod"); got != goModContent {
				t.Fatalf("go.mod changed on disk:\n%s", compare.Text(goModContent, got))
			}
		})
	})

	// Reproduce golang/go#40269 by deleting and recreating main.go.
	t.Run("delete main.go", func(t *testing.T) {
		runner.Run(t, untidyModule, func(t *testing.T, env *Env) {
			goModContent := env.ReadWorkspaceFile("a/go.mod")
			mainContent := env.ReadWorkspaceFile("a/main.go")
			env.OpenFile("a/main.go")
			env.SaveBuffer("a/main.go")

			// Ensure that we're done processing all the changes caused by opening
			// and saving above. If not, we may run into a file locking issue on
			// windows.
			//
			// If this proves insufficient, env.RemoveWorkspaceFile can be updated to
			// retry file lock errors on windows.
			env.AfterChange()
			env.RemoveWorkspaceFile("a/main.go")

			// TODO(rfindley): awaiting here shouldn't really be necessary. We should
			// be consistent eventually.
			//
			// Probably this was meant to exercise a race with the change below.
			env.AfterChange()

			env.WriteWorkspaceFile("a/main.go", mainContent)
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/main.go", "\"example.com/blah\"")),
			)
			if got := env.ReadWorkspaceFile("a/go.mod"); got != goModContent {
				t.Fatalf("go.mod changed on disk:\n%s", compare.Text(goModContent, got))
			}
		})
	})
}

func TestGoGetFix(t *testing.T) {
	const mod = `
-- a/go.mod --
module mod.com

go 1.12

-- a/main.go --
package main

import "example.com/blah"

var _ = blah.Name
`

	const want = `module mod.com

go 1.12

require example.com v1.2.3
`

	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, mod, func(t *testing.T, env *Env) {
		if strings.Contains(t.Name(), "workspace_module") {
			t.Skip("workspace module mode doesn't set -mod=readonly")
		}
		env.OpenFile("a/main.go")
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/main.go", `"example.com/blah"`)),
			ReadDiagnostics("a/main.go", &d),
		)
		var goGetDiag protocol.Diagnostic
		for _, diag := range d.Diagnostics {
			if strings.Contains(diag.Message, "could not import") {
				goGetDiag = diag
			}
		}
		env.ApplyQuickFixes("a/main.go", []protocol.Diagnostic{goGetDiag})
		if got := env.ReadWorkspaceFile("a/go.mod"); got != want {
			t.Fatalf("unexpected go.mod content:\n%s", compare.Text(want, got))
		}
	})
}

// Tests that multiple missing dependencies gives good single fixes.
func TestMissingDependencyFixes(t *testing.T) {
	const mod = `
-- a/go.mod --
module mod.com

go 1.12

-- a/main.go --
package main

import "example.com/blah"
import "random.org/blah"

var _, _ = blah.Name, hello.Name
`

	const want = `module mod.com

go 1.12

require random.org v1.2.3
`

	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("a/main.go")
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/main.go", `"random.org/blah"`)),
			ReadDiagnostics("a/main.go", &d),
		)
		var randomDiag protocol.Diagnostic
		for _, diag := range d.Diagnostics {
			if strings.Contains(diag.Message, "random.org") {
				randomDiag = diag
			}
		}
		env.ApplyQuickFixes("a/main.go", []protocol.Diagnostic{randomDiag})
		if got := env.ReadWorkspaceFile("a/go.mod"); got != want {
			t.Fatalf("unexpected go.mod content:\n%s", compare.Text(want, got))
		}
	})
}

// Tests that multiple missing dependencies gives good single fixes.
func TestMissingDependencyFixesWithGoWork(t *testing.T) {
	const mod = `
-- go.work --
go 1.18

use (
	./a
)
-- a/go.mod --
module mod.com

go 1.12

-- a/main.go --
package main

import "example.com/blah"
import "random.org/blah"

var _, _ = blah.Name, hello.Name
`

	const want = `module mod.com

go 1.12

require random.org v1.2.3
`

	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("a/main.go")
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/main.go", `"random.org/blah"`)),
			ReadDiagnostics("a/main.go", &d),
		)
		var randomDiag protocol.Diagnostic
		for _, diag := range d.Diagnostics {
			if strings.Contains(diag.Message, "random.org") {
				randomDiag = diag
			}
		}
		env.ApplyQuickFixes("a/main.go", []protocol.Diagnostic{randomDiag})
		if got := env.ReadWorkspaceFile("a/go.mod"); got != want {
			t.Fatalf("unexpected go.mod content:\n%s", compare.Text(want, got))
		}
	})
}

func TestIndirectDependencyFix(t *testing.T) {
	const mod = `
-- a/go.mod --
module mod.com

go 1.12

require example.com v1.2.3 // indirect
-- a/go.sum --
example.com v1.2.3 h1:ihBTGWGjTU3V4ZJ9OmHITkU9WQ4lGdQkMjgyLFk0FaY=
example.com v1.2.3/go.mod h1:Y2Rc5rVWjWur0h3pd9aEvK5Pof8YKDANh9gHA2Maujo=
-- a/main.go --
package main

import "example.com/blah"

func main() {
	fmt.Println(blah.Name)
`
	const want = `module mod.com

go 1.12

require example.com v1.2.3
`

	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("a/go.mod")
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/go.mod", "// indirect")),
			ReadDiagnostics("a/go.mod", &d),
		)
		env.ApplyQuickFixes("a/go.mod", d.Diagnostics)
		if got := env.BufferText("a/go.mod"); got != want {
			t.Fatalf("unexpected go.mod content:\n%s", compare.Text(want, got))
		}
	})
}

// Test to reproduce golang/go#39041. It adds a new require to a go.mod file
// that already has an unused require.
func TestNewDepWithUnusedDep(t *testing.T) {

	const proxy = `
-- github.com/esimov/caire@v1.2.5/go.mod --
module github.com/esimov/caire

go 1.12
-- github.com/esimov/caire@v1.2.5/caire.go --
package caire

func RemoveTempImage() {}
-- google.golang.org/protobuf@v1.20.0/go.mod --
module google.golang.org/protobuf

go 1.12
-- google.golang.org/protobuf@v1.20.0/hello/hello.go --
package hello
`
	const repro = `
-- a/go.mod --
module mod.com

go 1.14

require google.golang.org/protobuf v1.20.0
-- a/go.sum --
github.com/esimov/caire v1.2.5 h1:OcqDII/BYxcBYj3DuwDKjd+ANhRxRqLa2n69EGje7qw=
github.com/esimov/caire v1.2.5/go.mod h1:mXnjRjg3+WUtuhfSC1rKRmdZU9vJZyS1ZWU0qSvJhK8=
google.golang.org/protobuf v1.20.0 h1:y9T1vAtFKQg0faFNMOxJU7WuEqPWolVkjIkU6aI8qCY=
google.golang.org/protobuf v1.20.0/go.mod h1:FcqsytGClbtLv1ot8NvsJHjBi0h22StKVP+K/j2liKA=
-- a/main.go --
package main

import (
    "github.com/esimov/caire"
)

func _() {
    caire.RemoveTempImage()
}`

	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, repro, func(t *testing.T, env *Env) {
		env.OpenFile("a/main.go")
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/main.go", `"github.com/esimov/caire"`)),
			ReadDiagnostics("a/main.go", &d),
		)
		env.ApplyQuickFixes("a/main.go", d.Diagnostics)
		want := `module mod.com

go 1.14

require (
	github.com/esimov/caire v1.2.5
	google.golang.org/protobuf v1.20.0
)
`
		if got := env.ReadWorkspaceFile("a/go.mod"); got != want {
			t.Fatalf("TestNewDepWithUnusedDep failed:\n%s", compare.Text(want, got))
		}
	})
}

// TODO: For this test to be effective, the sandbox's file watcher must respect
// the file watching GlobPattern in the capability registration. See
// golang/go#39384.
func TestModuleChangesOnDisk(t *testing.T) {
	const mod = `
-- a/go.mod --
module mod.com

go 1.12

require example.com v1.2.3
-- a/go.sum --
example.com v1.2.3 h1:ihBTGWGjTU3V4ZJ9OmHITkU9WQ4lGdQkMjgyLFk0FaY=
example.com v1.2.3/go.mod h1:Y2Rc5rVWjWur0h3pd9aEvK5Pof8YKDANh9gHA2Maujo=
-- a/main.go --
package main

func main() {
	fmt.Println(blah.Name)
`
	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, mod, func(t *testing.T, env *Env) {
		// With zero-config gopls, we must open a/main.go to have a View including a/go.mod.
		env.OpenFile("a/main.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/go.mod", "require")),
		)
		env.RunGoCommandInDir("a", "mod", "tidy")
		env.AfterChange(
			NoDiagnostics(ForFile("a/go.mod")),
		)
	})
}

// Tests golang/go#39784: a missing indirect dependency, necessary
// due to blah@v2.0.0's incomplete go.mod file.
func TestBadlyVersionedModule(t *testing.T) {
	const proxy = `
-- example.com/blah/@v/v1.0.0.mod --
module example.com

go 1.12
-- example.com/blah@v1.0.0/blah.go --
package blah

const Name = "Blah"
-- example.com/blah/v2/@v/v2.0.0.mod --
module example.com

go 1.12
-- example.com/blah/v2@v2.0.0/blah.go --
package blah

import "example.com/blah"

var V1Name = blah.Name
const Name = "Blah"
`
	const files = `
-- a/go.mod --
module mod.com

go 1.12

require example.com/blah/v2 v2.0.0
-- a/go.sum --
example.com/blah v1.0.0 h1:kGPlWJbMsn1P31H9xp/q2mYI32cxLnCvauHN0AVaHnc=
example.com/blah v1.0.0/go.mod h1:PZUQaGFeVjyDmAE8ywmLbmDn3fj4Ws8epg4oLuDzW3M=
example.com/blah/v2 v2.0.0 h1:DNPsFPkKtTdxclRheaMCiYAoYizp6PuBzO0OmLOO0pY=
example.com/blah/v2 v2.0.0/go.mod h1:UZiKbTwobERo/hrqFLvIQlJwQZQGxWMVY4xere8mj7w=
-- a/main.go --
package main

import "example.com/blah/v2"

var _ = blah.Name
`
	RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}.Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("a/main.go")
		env.OpenFile("a/go.mod")
		var modDiags protocol.PublishDiagnosticsParams
		env.AfterChange(
			// We would like for the error to appear in the v2 module, but
			// as of writing non-workspace packages are not diagnosed.
			Diagnostics(env.AtRegexp("a/main.go", `"example.com/blah/v2"`), WithMessage("no required module provides")),
			Diagnostics(env.AtRegexp("a/go.mod", `require example.com/blah/v2`), WithMessage("no required module provides")),
			ReadDiagnostics("a/go.mod", &modDiags),
		)

		env.ApplyQuickFixes("a/go.mod", modDiags.Diagnostics)
		const want = `module mod.com

go 1.12

require (
	example.com/blah v1.0.0 // indirect
	example.com/blah/v2 v2.0.0
)
`
		env.SaveBuffer("a/go.mod")
		env.AfterChange(NoDiagnostics(ForFile("a/main.go")))
		if got := env.BufferText("a/go.mod"); got != want {
			t.Fatalf("suggested fixes failed:\n%s", compare.Text(want, got))
		}
	})
}

// Reproduces golang/go#38232.
func TestUnknownRevision(t *testing.T) {
	if runtime.GOOS == "plan9" {
		t.Skipf("skipping test that fails for unknown reasons on plan9; see https://go.dev/issue/50477")
	}
	const unknown = `
-- a/go.mod --
module mod.com

require (
	example.com v1.2.2
)
-- a/main.go --
package main

import "example.com/blah"

func main() {
	var x = blah.Name
}
`

	runner := RunMultiple{
		{"default", WithOptions(ProxyFiles(proxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(proxy))},
	}
	// Start from a bad state/bad IWL, and confirm that we recover.
	t.Run("bad", func(t *testing.T) {
		runner.Run(t, unknown, func(t *testing.T, env *Env) {
			env.OpenFile("a/go.mod")
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/go.mod", "example.com v1.2.2")),
			)
			env.RegexpReplace("a/go.mod", "v1.2.2", "v1.2.3")
			env.SaveBuffer("a/go.mod") // Save to trigger diagnostics.

			d := protocol.PublishDiagnosticsParams{}
			env.AfterChange(
				// Make sure the diagnostic mentions the new version -- the old diagnostic is in the same place.
				Diagnostics(env.AtRegexp("a/go.mod", "example.com v1.2.3"), WithMessage("example.com@v1.2.3")),
				ReadDiagnostics("a/go.mod", &d),
			)
			qfs := env.GetQuickFixes("a/go.mod", d.Diagnostics)
			if len(qfs) == 0 {
				t.Fatalf("got 0 code actions to fix %v, wanted at least 1", d.Diagnostics)
			}
			env.ApplyCodeAction(qfs[0]) // Arbitrarily pick a single fix to apply. Applying all of them seems to cause trouble in this particular test.
			env.SaveBuffer("a/go.mod")  // Save to trigger diagnostics.
			env.AfterChange(
				NoDiagnostics(ForFile("a/go.mod")),
				Diagnostics(env.AtRegexp("a/main.go", "x = ")),
			)
		})
	})

	const known = `
-- a/go.mod --
module mod.com

require (
	example.com v1.2.3
)
-- a/go.sum --
example.com v1.2.3 h1:ihBTGWGjTU3V4ZJ9OmHITkU9WQ4lGdQkMjgyLFk0FaY=
example.com v1.2.3/go.mod h1:Y2Rc5rVWjWur0h3pd9aEvK5Pof8YKDANh9gHA2Maujo=
-- a/main.go --
package main

import "example.com/blah"

func main() {
	var x = blah.Name
}
`
	// Start from a good state, transform to a bad state, and confirm that we
	// still recover.
	t.Run("good", func(t *testing.T) {
		runner.Run(t, known, func(t *testing.T, env *Env) {
			env.OpenFile("a/go.mod")
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/main.go", "x = ")),
			)
			env.RegexpReplace("a/go.mod", "v1.2.3", "v1.2.2")
			env.SaveBuffer("a/go.mod") // go.mod changes must be on disk
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/go.mod", "example.com v1.2.2")),
			)
			env.RegexpReplace("a/go.mod", "v1.2.2", "v1.2.3")
			env.SaveBuffer("a/go.mod") // go.mod changes must be on disk
			env.AfterChange(
				Diagnostics(env.AtRegexp("a/main.go", "x = ")),
			)
		})
	})
}

// Confirm that an error in an indirect dependency of a requirement is surfaced
// as a diagnostic in the go.mod file.
func TestErrorInIndirectDependency(t *testing.T) {
	const badProxy = `
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12

require random.org v1.2.3 // indirect
-- example.com@v1.2.3/blah/blah.go --
package blah

const Name = "Blah"
-- random.org@v1.2.3/go.mod --
module bob.org

go 1.12
-- random.org@v1.2.3/blah/blah.go --
package hello

const Name = "Hello"
`
	const module = `
-- a/go.mod --
module mod.com

go 1.14

require example.com v1.2.3
-- a/main.go --
package main

import "example.com/blah"

func main() {
	println(blah.Name)
}
`
	RunMultiple{
		{"default", WithOptions(ProxyFiles(badProxy), WorkspaceFolders("a"))},
		{"nested", WithOptions(ProxyFiles(badProxy))},
	}.Run(t, module, func(t *testing.T, env *Env) {
		env.OpenFile("a/go.mod")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/go.mod", "require example.com v1.2.3")),
		)
	})
}

// A copy of govim's config_set_env_goflags_mod_readonly test.
func TestGovimModReadonly(t *testing.T) {
	const mod = `
-- go.mod --
module mod.com

go 1.13
-- main.go --
package main

import "example.com/blah"

func main() {
	println(blah.Name)
}
`
	WithOptions(
		EnvVars{"GOFLAGS": "-mod=readonly"},
		ProxyFiles(proxy),
		Modes(Default),
	).Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("main.go")
		original := env.ReadWorkspaceFile("go.mod")
		env.AfterChange(
			Diagnostics(env.AtRegexp("main.go", `"example.com/blah"`)),
		)
		got := env.ReadWorkspaceFile("go.mod")
		if got != original {
			t.Fatalf("go.mod file modified:\n%s", compare.Text(original, got))
		}
		env.RunGoCommand("get", "example.com/blah@v1.2.3")
		env.RunGoCommand("mod", "tidy")
		env.AfterChange(
			NoDiagnostics(ForFile("main.go")),
		)
	})
}

func TestMultiModuleModDiagnostics(t *testing.T) {
	const mod = `
-- go.work --
go 1.18

use (
	a
	b
)
-- a/go.mod --
module moda.com

go 1.14

require (
	example.com v1.2.3
)
-- a/go.sum --
example.com v1.2.3 h1:Yryq11hF02fEf2JlOS2eph+ICE2/ceevGV3C9dl5V/c=
example.com v1.2.3/go.mod h1:Y2Rc5rVWjWur0h3pd9aEvK5Pof8YKDANh9gHA2Maujo=
-- a/main.go --
package main

func main() {}
-- b/go.mod --
module modb.com

require example.com v1.2.3

go 1.14
-- b/main.go --
package main

import "example.com/blah"

func main() {
	blah.SaySomething()
}
`
	WithOptions(
		ProxyFiles(workspaceProxy),
	).Run(t, mod, func(t *testing.T, env *Env) {
		env.AfterChange(
			Diagnostics(
				env.AtRegexp("a/go.mod", "example.com v1.2.3"),
				WithMessage("is not used"),
			),
		)
	})
}

func TestModTidyWithBuildTags(t *testing.T) {
	const mod = `
-- go.mod --
module mod.com

go 1.14
-- main.go --
// +build bob

package main

import "example.com/blah"

func main() {
	blah.SaySomething()
}
`
	WithOptions(
		ProxyFiles(workspaceProxy),
		Settings{"buildFlags": []string{"-tags", "bob"}},
	).Run(t, mod, func(t *testing.T, env *Env) {
		env.OnceMet(
			InitialWorkspaceLoad,
			Diagnostics(env.AtRegexp("main.go", `"example.com/blah"`)),
		)
	})
}

func TestModTypoDiagnostic(t *testing.T) {
	const mod = `
-- go.mod --
module mod.com

go 1.12
-- main.go --
package main

func main() {}
`
	Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("go.mod")
		env.RegexpReplace("go.mod", "module", "modul")
		env.AfterChange(
			Diagnostics(env.AtRegexp("go.mod", "modul")),
		)
	})
}

func TestSumUpdateFixesDiagnostics(t *testing.T) {
	const mod = `
-- go.mod --
module mod.com

go 1.12

require (
	example.com v1.2.3
)
-- main.go --
package main

import (
	"example.com/blah"
)

func main() {
	println(blah.Name)
}
`
	WithOptions(
		ProxyFiles(workspaceProxy),
	).Run(t, mod, func(t *testing.T, env *Env) {
		d := &protocol.PublishDiagnosticsParams{}
		env.OpenFile("go.mod")
		env.AfterChange(
			Diagnostics(
				env.AtRegexp("go.mod", `example.com v1.2.3`),
				WithMessage("go.sum is out of sync"),
			),
			ReadDiagnostics("go.mod", d),
		)
		env.ApplyQuickFixes("go.mod", d.Diagnostics)
		env.SaveBuffer("go.mod") // Save to trigger diagnostics.
		env.AfterChange(
			NoDiagnostics(ForFile("go.mod")),
		)
	})
}

// This test confirms that editing a go.mod file only causes metadata
// to be invalidated when it's saved.
func TestGoModInvalidatesOnSave(t *testing.T) {
	const mod = `
-- go.mod --
module mod.com

go 1.12
-- main.go --
package main

func main() {
	hello()
}
-- hello.go --
package main

func hello() {}
`
	WithOptions(
		// TODO(rFindley) this doesn't work in multi-module workspace mode, because
		// it keeps around the last parsing modfile. Update this test to also
		// exercise the workspace module.
		Modes(Default),
	).Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("go.mod")
		env.Await(env.DoneWithOpen())
		env.RegexpReplace("go.mod", "module", "modul")
		// Confirm that we still have metadata with only on-disk edits.
		env.OpenFile("main.go")
		loc := env.FirstDefinition(env.RegexpSearch("main.go", "hello"))
		if loc.URI.Base() != "hello.go" {
			t.Fatalf("expected definition in hello.go, got %s", loc.URI)
		}
		// Confirm that we no longer have metadata when the file is saved.
		env.SaveBufferWithoutActions("go.mod")
		_, err := env.Editor.Definitions(env.Ctx, env.RegexpSearch("main.go", "hello"))
		if err == nil {
			t.Fatalf("expected error, got none")
		}
	})
}

func TestRemoveUnusedDependency(t *testing.T) {
	const proxy = `
-- hasdep.com@v1.2.3/go.mod --
module hasdep.com

go 1.12

require example.com v1.2.3
-- hasdep.com@v1.2.3/a/a.go --
package a
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12
-- example.com@v1.2.3/blah/blah.go --
package blah

const Name = "Blah"
-- random.com@v1.2.3/go.mod --
module random.com

go 1.12
-- random.com@v1.2.3/blah/blah.go --
package blah

const Name = "Blah"
`
	t.Run("almost tidied", func(t *testing.T) {
		const mod = `
-- go.mod --
module mod.com

go 1.12

require hasdep.com v1.2.3
-- main.go --
package main

func main() {}
`
		WithOptions(
			ProxyFiles(proxy),
		).Run(t, mod, func(t *testing.T, env *Env) {
			env.OpenFile("go.mod")
			d := &protocol.PublishDiagnosticsParams{}
			env.AfterChange(
				Diagnostics(env.AtRegexp("go.mod", "require hasdep.com v1.2.3")),
				ReadDiagnostics("go.mod", d),
			)
			const want = `module mod.com

go 1.12
`
			env.ApplyQuickFixes("go.mod", d.Diagnostics)
			if got := env.BufferText("go.mod"); got != want {
				t.Fatalf("unexpected content in go.mod:\n%s", compare.Text(want, got))
			}
		})
	})

	t.Run("not tidied", func(t *testing.T) {
		const mod = `
-- go.mod --
module mod.com

go 1.12

require hasdep.com v1.2.3
require random.com v1.2.3
-- main.go --
package main

func main() {}
`
		WithOptions(
			WriteGoSum("."),
			ProxyFiles(proxy),
		).Run(t, mod, func(t *testing.T, env *Env) {
			d := &protocol.PublishDiagnosticsParams{}
			env.OpenFile("go.mod")
			pos := env.RegexpSearch("go.mod", "require hasdep.com v1.2.3").Range.Start
			env.AfterChange(
				Diagnostics(AtPosition("go.mod", pos.Line, pos.Character)),
				ReadDiagnostics("go.mod", d),
			)
			const want = `module mod.com

go 1.12

require random.com v1.2.3
`
			var diagnostics []protocol.Diagnostic
			for _, d := range d.Diagnostics {
				if d.Range.Start.Line != pos.Line {
					continue
				}
				diagnostics = append(diagnostics, d)
			}
			env.ApplyQuickFixes("go.mod", diagnostics)
			if got := env.BufferText("go.mod"); got != want {
				t.Fatalf("unexpected content in go.mod:\n%s", compare.Text(want, got))
			}
		})
	})
}

func TestSumUpdateQuickFix(t *testing.T) {
	const mod = `
-- go.mod --
module mod.com

go 1.12

require (
	example.com v1.2.3
)
-- main.go --
package main

import (
	"example.com/blah"
)

func main() {
	blah.Hello()
}
`
	WithOptions(
		ProxyFiles(workspaceProxy),
		Modes(Default),
	).Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("go.mod")
		params := &protocol.PublishDiagnosticsParams{}
		env.AfterChange(
			Diagnostics(
				env.AtRegexp("go.mod", `example.com`),
				WithMessage("go.sum is out of sync"),
			),
			ReadDiagnostics("go.mod", params),
		)
		env.ApplyQuickFixes("go.mod", params.Diagnostics)
		const want = `example.com v1.2.3 h1:Yryq11hF02fEf2JlOS2eph+ICE2/ceevGV3C9dl5V/c=
example.com v1.2.3/go.mod h1:Y2Rc5rVWjWur0h3pd9aEvK5Pof8YKDANh9gHA2Maujo=
`
		if got := env.ReadWorkspaceFile("go.sum"); got != want {
			t.Fatalf("unexpected go.sum contents:\n%s", compare.Text(want, got))
		}
	})
}

func TestDownloadDeps(t *testing.T) {
	const proxy = `
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12

require random.org v1.2.3
-- example.com@v1.2.3/blah/blah.go --
package blah

import "random.org/bye"

func SaySomething() {
	bye.Goodbye()
}
-- random.org@v1.2.3/go.mod --
module random.org

go 1.12
-- random.org@v1.2.3/bye/bye.go --
package bye

func Goodbye() {
	println("Bye")
}
`

	const mod = `
-- go.mod --
module mod.com

go 1.12
-- main.go --
package main

import (
	"example.com/blah"
)

func main() {
	blah.SaySomething()
}
`
	WithOptions(
		ProxyFiles(proxy),
		Modes(Default),
	).Run(t, mod, func(t *testing.T, env *Env) {
		env.OpenFile("main.go")
		d := &protocol.PublishDiagnosticsParams{}
		env.AfterChange(
			Diagnostics(
				env.AtRegexp("main.go", `"example.com/blah"`),
				WithMessage(`could not import example.com/blah (no required module provides package "example.com/blah")`),
			),
			ReadDiagnostics("main.go", d),
		)
		env.ApplyQuickFixes("main.go", d.Diagnostics)
		env.AfterChange(
			NoDiagnostics(ForFile("main.go")),
			NoDiagnostics(ForFile("go.mod")),
		)
	})
}

func TestInvalidGoVersion(t *testing.T) {
	const files = `
-- go.mod --
module mod.com

go foo
-- main.go --
package main
`
	Run(t, files, func(t *testing.T, env *Env) {
		env.OnceMet(
			InitialWorkspaceLoad,
			Diagnostics(env.AtRegexp("go.mod", `go foo`), WithMessage("invalid go version")),
		)
		env.WriteWorkspaceFile("go.mod", "module mod.com \n\ngo 1.12\n")
		env.AfterChange(NoDiagnostics(ForFile("go.mod")))
	})
}

// This is a regression test for a bug in the line-oriented implementation
// of the "apply diffs" operation used by the fake editor.
func TestIssue57627(t *testing.T) {
	const files = `
-- go.work --
package main
`
	Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("go.work")
		env.SetBufferContent("go.work", "go 1.18\nuse moda/a")
		env.SaveBuffer("go.work") // doesn't fail
	})
}

func TestInconsistentMod(t *testing.T) {
	const proxy = `
-- golang.org/x/mod@v0.7.0/go.mod --
go 1.20
module golang.org/x/mod
-- golang.org/x/mod@v0.7.0/a.go --
package mod
func AutoQuote(string) string { return ""}
-- golang.org/x/mod@v0.9.0/go.mod --
go 1.20
module golang.org/x/mod
-- golang.org/x/mod@v0.9.0/a.go --
package mod
func AutoQuote(string) string { return ""}
`
	const files = `
-- go.work --
go 1.20
use (
	./a
	./b
)

-- a/go.mod --
module a.mod.com
go 1.20
require golang.org/x/mod v0.6.0 // yyy
replace golang.org/x/mod v0.6.0 => golang.org/x/mod v0.7.0
-- a/main.go --
package main
import "golang.org/x/mod"
import "fmt"
func main() {fmt.Println(mod.AutoQuote(""))}

-- b/go.mod --
module b.mod.com
go 1.20
require golang.org/x/mod v0.9.0 // xxx
-- b/main.go --
package aaa
import "golang.org/x/mod"
import "fmt"
func main() {fmt.Println(mod.AutoQuote(""))}
var A int

-- b/c/go.mod --
module c.b.mod.com
go 1.20
require b.mod.com v0.4.2
replace b.mod.com => ../
-- b/c/main.go --
package main
import "b.mod.com/aaa"
import "fmt"
func main() {fmt.Println(aaa.A)}
`
	WithOptions(
		ProxyFiles(proxy),
		Modes(Default),
	).Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("a/go.mod")
		ahints := env.InlayHints("a/go.mod")
		if len(ahints) != 1 {
			t.Errorf("expected exactly one hint, got %d: %#v", len(ahints), ahints)
		}
		env.OpenFile("b/c/go.mod")
		bhints := env.InlayHints("b/c/go.mod")
		if len(bhints) != 0 {
			t.Errorf("expected no hints, got %d: %#v", len(bhints), bhints)
		}
	})

}
