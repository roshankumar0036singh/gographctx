#!/usr/bin/env python3
"""Optional Jedi static-analyzer oracle for generating internal CallEdgeTruth JSON for GoGraphCtx."""
import json
import os
import sys

def main():
    if len(sys.argv) < 2:
        print("usage: jedi_oracle.py <repo_root> [out.json]", file=sys.stderr)
        sys.exit(2)
    try:
        import jedi
    except ImportError:
        print("jedi not installed; run `pip install jedi`", file=sys.stderr)
        sys.exit(1)

    root = os.path.abspath(sys.argv[1])
    proj = jedi.Project(root)
    edges = []
    seen = set()

    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in {".git", ".ggctx", "__pycache__", ".venv", "venv"}]
        for fn in sorted(filenames):
            if not fn.endswith(".py"):
                continue
            abs_p = os.path.join(dirpath, fn)
            rel_p = os.path.relpath(abs_p, root).replace("\\", "/")
            try:
                src = open(abs_p, "r", encoding="utf-8", errors="ignore").read()
                script = jedi.Script(src, path=abs_p, project=proj)
                names = script.get_names(all_scopes=True, references=True)
            except Exception:
                continue
            for n in names:
                if not n.is_definition() and n.type in {"statement", "expression"}:
                    continue
            # Extract references and resolve via goto() inside repo root.
            for n in names:
                if n.is_definition():
                    continue
                try:
                    defs = n.goto(follow_imports=True)
                except Exception:
                    continue
                for d in defs:
                    if not d.module_path:
                        continue
                    mp = os.path.abspath(str(d.module_path))
                    if not mp.startswith(root):
                        continue
                    t_rel = os.path.relpath(mp, root).replace("\\", "/")
                    if d.type not in {"function", "class"}:
                        continue
                    ctx = n.parent()
                    caller = ctx.name if ctx and ctx.type in {"function", "class"} else "<module>"
                    key = (rel_p, caller, t_rel, d.name)
                    if key in seen:
                        continue
                    seen.add(key)
                    edges.append({
                        "caller_file": rel_p,
                        "caller_name": caller,
                        "callee_file": t_rel,
                        "callee_name": d.name,
                        "line": n.line or 1,
                    })

    out = json.dumps(edges, indent=2)
    if len(sys.argv) >= 3:
        with open(sys.argv[2], "w", encoding="utf-8") as f:
            f.write(out + "\n")
    else:
        print(out)

if __name__ == "__main__":
    main()
