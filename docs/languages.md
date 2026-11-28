# Supported Languages & Resolution Capabilities

All 6 core languages are parsed via compiled-in **pure-Go `gotreesitter` grammars** (`CGO_ENABLED=0`—zero runtime C/Python dependencies).

| Language | Extensions | Imports & Modules | Receiver Type Inference | Links (`INHERITS` / `IMPLEMENTS` / `DECORATED_BY`) |
|---|---|---|---|---|
| **Go** | `.go` | `go.mod` module prefix + local `replace` directives, aliases, dot imports | Short/long `var` declarations, constructors, struct literals, chained calls, embedded struct field promotion | Embedded structs (`INHERITS`), automatic structural interface satisfaction (`IMPLEMENTS`) |
| **Python** | `.py`, `.pyi` | Relative dots (`.`, `..`), package `__init__.py`, `from x import y as z` | Parameter annotations, local annotated/unannotated assignments, class instantiation, constructor `self` | Class bases (`INHERITS`), `Protocol` / `ABC` (`IMPLEMENTS`), `@decorator` targets (`DECORATED_BY`) |
| **TypeScript / TSX** | `.ts`, `.tsx`, `.mts`, `.cts` | `tsconfig.json` `paths` + `baseUrl`, `package.json`, barrel `index.ts` re-exports | Type annotations, `new Class()`, `as` / generic instantiation, typed factories | `extends` (`INHERITS`), `implements` (`IMPLEMENTS`), `@decorator` (`DECORATED_BY`) |
| **JavaScript / JSX** | `.js`, `.jsx`, `.mjs`, `.cjs` | Relative ESM/CommonJS `require` + `package.json` `exports` / `main` | Class constructors, `new`, return inference | `extends` (`INHERITS`), `@decorator` (`DECORATED_BY`) |
| **Rust** | `.rs` | `Cargo.toml` crate name + workspace members, `crate::`, `super::`, `self::`, `mod.rs` / `use ...::{...}` | Let bindings with type annotations, `::new()` constructors, `Result<T, E>` / `Option<T>` unwrapping, `impl Trait for Struct` | `impl Trait for Struct` (`IMPLEMENTS`), supertraits (`INHERITS`) |
| **Java** | `.java` | Standard package roots (`src/main/java`, etc.), single/wildcard imports, static imports, inner classes | Declared local variable types, `new`, static factory chains, arity overload filtering | `extends` (`INHERITS`), `implements` (`IMPLEMENTS`), `@Annotation` (`DECORATED_BY`) |
