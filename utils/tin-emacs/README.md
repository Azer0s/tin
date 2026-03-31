# tin-mode.el
Emacs major mode for the [Tin](https://github.com/ariel/tin) programming language.
Based on [rexim/simpc-mode](https://github.com/rexim/simpc-mode).
## Features
- Syntax highlighting
  - Keywords (`let`, `fn`, `struct`, `trait`, `enum`, `union`, `macro`, ...)
  - Built-in types (`i8`-`i64`, `u8`-`u64`, `f32`/`f64`, `bool`, `string`, `atom`, `void`, `any`)
  - Constants (`true`, `false`, `nil`)
  - Atom literals (`'ok`, `'err`, ...)
  - Control tags (`#pure`, `#no_recurse`, `#sideffect`, ...)
  - Function / macro declaration names
  - Type declaration names (`struct`, `trait`, `enum`, `union`, `type`)
  - Namespace paths (`module::item`)
- Line comments (`//`)
- Indentation
  - Blocks open with `=` or `:` at end of line (2-space indent)
  - `else` dedents back to the matching `if` level
  - `case` / `default` indent from `match:` or dedent from a case body
- `auto-mode-alist` entry for `*.tin`
## Installation
### Manual
Copy `tin-mode.el` somewhere on your `load-path`, then add to your config:
```elisp
(require 'tin-mode)
```
### use-package (local)
```elisp
(use-package tin-mode
  :load-path \path/to/tin/utils/tin-emacs\)
```
### straight.el
```elisp
(straight-use-package
  '(tin-mode :type git :local-repo \path/to/tin/utils/tin-emacs\))
```
