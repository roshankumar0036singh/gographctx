package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenerateFlaskScalePythonCorpus writes a deterministic Flask-shaped multi-module
// Python repository (default 83 .py files) with classes, decorators, inheritance,
// typed helpers, and cross-file call chains for speed and accuracy benchmarking.
func GenerateFlaskScalePythonCorpus(root string, numFiles int) error {
	if numFiles <= 0 {
		numFiles = 83
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		return err
	}

	files := map[string]string{
		"app/__init__.py": `from .config import Settings
from .app_core import FlaskApp

def create_app(env: str = "prod") -> FlaskApp:
    cfg = Settings(env)
    app = FlaskApp(cfg)
    return app
`,
		"app/config.py": `class BaseConfig:
    def is_debug(self) -> bool:
        return False

class Settings(BaseConfig):
    def __init__(self, env: str) -> None:
        self.env = env

    def dsn(self) -> str:
        return f"sqlite:///{self.env}.db"
`,
		"app/helpers.py": `def clean_input(raw: str) -> str:
    return raw.strip()

def sign_token(payload: str) -> str:
    return f"sig:{payload}"

def audit_event(name: str, detail: str) -> str:
    cleaned = clean_input(detail)
    return sign_token(f"{name}:{cleaned}")
`,
		"app/app_core.py": `from .config import Settings
from .helpers import audit_event

class RequestContext:
    def __init__(self, path: str) -> None:
        self.path = path

class FlaskApp:
    def __init__(self, settings: Settings) -> None:
        self.settings = settings

    def dispatch(self, path: str) -> str:
        ctx = RequestContext(path)
        return audit_event("dispatch", ctx.path)
`,
	}

	// Pad up to numFiles across core/, blueprints/, services/, and models/.
	subpkgs := []string{"core", "blueprints", "services", "models"}
	for _, pkg := range subpkgs {
		initRel := fmt.Sprintf("app/%s/__init__.py", pkg)
		files[initRel] = fmt.Sprintf("# %s package\n", pkg)
	}

	idx := 0
	for len(files) < numFiles {
		pkg := subpkgs[idx%len(subpkgs)]
		modName := fmt.Sprintf("mod_%02d", idx)
		rel := fmt.Sprintf("app/%s/%s.py", pkg, modName)

		var sb strings.Builder
		sb.WriteString("from ..helpers import clean_input, sign_token, audit_event\n")
		sb.WriteString("from ..config import Settings\n\n")
		fmt.Fprintf(&sb, "class %sService:\n", strings.ToUpper(pkg[:1])+pkg[1:])
		sb.WriteString("    def __init__(self, cfg: Settings) -> None:\n")
		sb.WriteString("        self.cfg = cfg\n\n")
		for m := 0; m < 4; m++ {
			fmt.Fprintf(&sb, "    def handle_%02d_%d(self, item: str) -> str:\n", idx, m)
			fmt.Fprintf(&sb, "        norm = clean_input(item)\n")
			fmt.Fprintf(&sb, "        if norm:\n")
			fmt.Fprintf(&sb, "            return audit_event(%q, norm)\n", fmt.Sprintf("%s_%d", modName, m))
			fmt.Fprintf(&sb, "        return sign_token(self.cfg.dsn())\n\n")
		}
		fmt.Fprintf(&sb, "def run_%s(value: str) -> str:\n", modName)
		fmt.Fprintf(&sb, "    svc = %sService(Settings(\"bench\"))\n", strings.ToUpper(pkg[:1])+pkg[1:])
		fmt.Fprintf(&sb, "    return svc.handle_%02d_0(value)\n", idx)

		files[rel] = sb.String()
		idx++
	}

	return writeCorpusFiles(root, files)
}

// GenerateGoMonorepoCorpus writes a deterministic multi-package Go module
// (default 1,000 .go files across 50 packages) for large-scale indexing benchmarks.
func GenerateGoMonorepoCorpus(root string, numFiles int) error {
	if numFiles <= 0 {
		numFiles = 1000
	}
	const numPkgs = 50
	files := make(map[string]string, numFiles+2)
	files["go.mod"] = "module example.com/ggctxbench\n\ngo 1.22\n"
	files["internal/shared/shared.go"] = `package shared

type Logger interface {
	Log(msg string) string
}

type StdLogger struct {
	Prefix string
}

func (l *StdLogger) Log(msg string) string {
	return l.Prefix + ":" + msg
}

func Normalize(v string) string {
	return v
}
`
	for i := 0; i < numFiles-1; i++ {
		pkgIdx := i % numPkgs
		pkgName := fmt.Sprintf("pkg%02d", pkgIdx)
		rel := fmt.Sprintf("internal/%s/file_%04d.go", pkgName, i)
		typeName := fmt.Sprintf("Worker%04d", i)
		var sb strings.Builder
		fmt.Fprintf(&sb, "package %s\n\n", pkgName)
		sb.WriteString("import \"example.com/ggctxbench/internal/shared\"\n\n")
		fmt.Fprintf(&sb, "type %s struct {\n\tLog shared.Logger\n}\n\n", typeName)
		fmt.Fprintf(&sb, "func New%s() *%s {\n\treturn &%s{Log: &shared.StdLogger{Prefix: %q}}\n}\n\n", typeName, typeName, typeName, typeName)
		fmt.Fprintf(&sb, "func (w *%s) Process(id int, raw string) string {\n", typeName)
		sb.WriteString("\tnorm := shared.Normalize(raw)\n")
		sb.WriteString("\tif id > 0 {\n\t\treturn w.Log.Log(norm)\n\t}\n")
		sb.WriteString("\treturn norm\n}\n")
		files[rel] = sb.String()
	}
	return writeCorpusFiles(root, files)
}

// GenerateMixedPolyglotCorpus creates a compact polyglot repo (Go + Python + TypeScript)
// suitable for cross-tool comparison benchmarks.
func GenerateMixedPolyglotCorpus(root string) error {
	files := map[string]string{
		"go.mod": "module example.com/polybench\n\ngo 1.22\n",
		"backend/store/store.go": `package store

type Store struct{}

func NewStore() *Store { return &Store{} }

func (s *Store) Save(id string) error { return nil }
`,
		"backend/api/handler.go": `package api

import "example.com/polybench/backend/store"

func HandleCreate(s *store.Store, id string) error {
	return s.Save(id)
}
`,
		"py/core.py": `def validate_payload(raw: str) -> str:
    return raw.strip()
`,
		"py/service.py": `from .core import validate_payload

class JobRunner:
    def run(self, raw: str) -> str:
        return validate_payload(raw)
`,
		"web/fmt.ts": `export function formatLabel(name: string): string {
    return name.trim();
}
`,
		"web/view.ts": `import { formatLabel } from "./fmt";

export class Panel {
    render(title: string): string {
        return formatLabel(title);
    }
}
`,
	}
	return writeCorpusFiles(root, files)
}

func writeCorpusFiles(root string, files map[string]string) error {
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}
