;;; tin-mode.el --- Major mode for the Tin programming language  -*- lexical-binding: t; -*-

;; Based on simpc-mode by rexim (https://github.com/rexim/simpc-mode)

;;; Commentary:

;; A simple major mode for the Tin programming language.
;; Features:
;;   - Syntax highlighting for keywords, types, constants, atoms,
;;     control tags, function/type declarations, macros, namespaces
;;   - Line comments (//) and block comments (/* */)
;;   - Indentation: blocks open with = or : at end of line (2-space)
;;   - auto-mode-alist for *.tin

;;; Code:

(require 'subr-x)

;;;; Syntax table

(defvar tin-mode-syntax-table
  (let ((table (make-syntax-table)))
    ;; Line comments (//) and block comments (/* */)
    (modify-syntax-entry ?/ ". 124b" table)
    (modify-syntax-entry ?* ". 23" table)
    (modify-syntax-entry ?\n "> b" table)
    ;; String literals
    (modify-syntax-entry ?\" "\"" table)
    ;; Single quote is punctuation - atoms use 'ident, not a string delimiter
    (modify-syntax-entry ?' "." table)
    ;; # is punctuation (control tags, not a preprocessor line)
    (modify-syntax-entry ?# "." table)
    ;; Underscore is a word constituent
    (modify-syntax-entry ?_ "w" table)
    ;; Operators
    (modify-syntax-entry ?+ "." table)
    (modify-syntax-entry ?- "." table)
    (modify-syntax-entry ?% "." table)
    (modify-syntax-entry ?& "." table)
    (modify-syntax-entry ?| "." table)
    (modify-syntax-entry ?^ "." table)
    (modify-syntax-entry ?! "." table)
    (modify-syntax-entry ?= "." table)
    (modify-syntax-entry ?< "." table)
    (modify-syntax-entry ?> "." table)
    table)
  "Syntax table for `tin-mode'.")

;;;; Token lists

(defun tin-keywords ()
  "Tin language keywords."
  '("let" "const" "fn" "type" "struct" "trait" "enum" "union"
    "use" "from" "export" "extern" "return" "if" "else" "for" "in"
    "match" "case" "default" "defer" "where" "macro" "static"
    "virtual" "as" "is" "forward" "override" "sizeof" "addr"
    "break" "do" "echo" "test" "typeof" "traitof" "fieldnames"
    "fieldtypes" "fieldtag" "getfield" "setfield" "pass" "isrc"
    "var" "spawn" "await" "yield" "weak"))

(defun tin-builtin-types ()
  "Tin built-in primitive types."
  '("i8" "i16" "i32" "i64" "i128"
    "u8" "u16" "u32" "u64" "u128"
    "f32" "f64" "f128"
    "bool" "string" "atom" "void" "any"))

(defun tin-constants ()
  "Tin language constants."
  '("true" "false" "nil"))

;;;; Imported macro tracking

(defvar-local tin--imported-macros :unset
  "Cache of macro names imported via `use { ... } from ...', or :unset if stale.")

(defun tin--collect-imported-macros ()
  "Scan buffer for `use { ... } from' and return a list of imported names."
  (let (result)
    (save-excursion
      (goto-char (point-min))
      (while (re-search-forward
              "\\buse\\s-*{\\([^}]*\\)}\\s-*from\\b" nil t)
        (dolist (tok (split-string (match-string-no-properties 1) "," t))
          (let ((name (string-trim (string-trim-right (string-trim tok) "!"))))
            (when (string-match-p "^[a-zA-Z_][a-zA-Z0-9_]*$" name)
              (push name result))))))
    result))

(defun tin--invalidate-macro-cache (&rest _)
  "Mark the imported-macro cache as stale."
  (setq-local tin--imported-macros :unset))

(defun tin--macro-matcher (limit)
  "Font-lock matcher for names imported via `use { ... } from ...'.
Only matches NAME! (with bang); plain NAME() is left to the function-call rule."
  (when (eq tin--imported-macros :unset)
    (setq-local tin--imported-macros (tin--collect-imported-macros)))
  (when tin--imported-macros
    (re-search-forward
     (concat "\\<\\(" (mapconcat #'regexp-quote tin--imported-macros "\\|") "\\)\\(!\\)")
     limit t)))

;;;; Font-lock

(defun tin-font-lock-keywords ()
  "Font-lock keyword list for `tin-mode'."
  (list
   ;; Control tags: #pure  #no_recurse  #sideffect  #allow_sideffect ...
   `("\\(#[a-zA-Z_][a-zA-Z0-9_]*\\)" . font-lock-preprocessor-face)

   ;; Quoted atom literals: '"content"
   `("'\"[^\"\n]*\"" . font-lock-constant-face)

   ;; Simple atom literals: 'ok  'err  'some_atom
   `("\\('[a-zA-Z_][a-zA-Z0-9_]*\\)" . font-lock-constant-face)

   ;; Keywords
   `(,(regexp-opt (tin-keywords) 'symbols) . font-lock-keyword-face)

   ;; Built-in types
   `(,(regexp-opt (tin-builtin-types) 'symbols) . font-lock-type-face)

   ;; Constants: true  false  nil
   `(,(regexp-opt (tin-constants) 'symbols) . font-lock-constant-face)

   ;; Function declaration:
   ;;   fn name(...)
   ;;   fn{#pure} name(...)
   ;;   fn[T] name(...)
   `("\\bfn\\(?:{[^}]*}\\)?\\(?:\\[[^]]*\\]\\)?\\s-+\\([a-zA-Z_][a-zA-Z0-9_]*\\)"
     (1 font-lock-function-name-face))

   ;; Macro definition: macro name!(...)
   `("\\bmacro\\s-+\\([a-zA-Z_][a-zA-Z0-9_]*\\)!"
     (1 font-lock-function-name-face))

   ;; Macro call: name!(...)
   `("\\b\\([a-zA-Z_][a-zA-Z0-9_]*\\)!("
     (1 font-lock-function-name-face))

   ;; Type declarations: struct/trait/enum/union/type [atom] Name
   `("\\b\\(?:struct\\|trait\\|enum\\|union\\|type\\)\\(?:\\s-+atom\\)?\\s-+\\([a-zA-Z_][a-zA-Z0-9_]*\\)"
     (1 font-lock-type-face))

   ;; Function calls: name(
   `("\\b\\([a-zA-Z_][a-zA-Z0-9_]*\\)\\s-*(" (1 font-lock-function-name-face))

   ;; Namespace access: module::item  (highlight the namespace part)
   `("\\b\\([a-zA-Z_][a-zA-Z0-9_]*\\)::[a-zA-Z_]"
     (1 font-lock-constant-face))

   ;; Names imported via use { x, y! } from ... highlighted as macro calls
   ;; group 1 = name, group 2 = !; plain name() is left to function-call rule above
   '(tin--macro-matcher
     (1 font-lock-function-name-face)
     (2 font-lock-function-name-face))))

;;;; Indentation
;;
;; Tin opens blocks with = or : at the end of a line (like Python uses :).
;; Rules:
;;   - prev line ends with = or : -> indent in by indent-len
;;   - current line starts with else -> dedent (back to the if level)
;;   - current line starts with case or default -> indent in from match:,
;;     or dedent from a case body
;;   - otherwise -> same indent as previous non-empty line

(defun tin--previous-non-empty-line ()
  "Return (LINE . INDENTATION) for the nearest previous non-empty line, or nil."
  (save-excursion
    (move-beginning-of-line nil)
    (if (bobp)
        nil
      (forward-line -1)
      (while (and (not (bobp))
                  (string-empty-p
                   (string-trim-right (thing-at-point 'line t))))
        (forward-line -1))
      (if (string-empty-p (string-trim-right (thing-at-point 'line t)))
          nil
        (cons (thing-at-point 'line t)
              (current-indentation))))))

(defun tin--desired-indentation ()
  "Compute the desired indentation for the current line."
  (let ((prev (tin--previous-non-empty-line)))
    (if (not prev)
        (current-indentation)
      (let* ((indent-len   2)
             (cur-line     (string-trim-right (thing-at-point 'line t)))
             (prev-line    (string-trim-right (car prev)))
             (prev-indent  (cdr prev))
             (cur-trimmed  (string-trim-left cur-line))
             ;; Does the previous line open a new block?
             (prev-opens   (or (string-suffix-p "=" prev-line)
                               (string-suffix-p ":" prev-line))))
        (cond
         ;; else / else if - dedent to the matching if level
         ((string-prefix-p "else" cur-trimmed)
          (if prev-opens
              (+ prev-indent indent-len)   ; prev was already a block opener
            (max (- prev-indent indent-len) 0)))

         ;; case / default - indent in from match:, or dedent from case body
         ((string-match-p "^\\(?:case\\|default\\)\\b" cur-trimmed)
          (if prev-opens
              (+ prev-indent indent-len)
            (max (- prev-indent indent-len) 0)))

         ;; previous line opened a block with = or :
         (prev-opens (+ prev-indent indent-len))

         ;; same level as previous line
         (t prev-indent))))))

(defun tin-indent-line ()
  "Indent the current line as Tin source."
  (interactive)
  (when (not (bobp))
    (let* ((desired (tin--desired-indentation))
           (n (max (- (current-column) (current-indentation)) 0)))
      (indent-line-to desired)
      (forward-char n))))

;;;; File icon support

(defun tin--icon (&rest _)
  (propertize "🥫" 'face '(:height 1.0) 'font-lock-face '(:height 1.0)
              'display '(raise -0.1) 'rear-nonsticky t))
;; all-the-icons calls `(fn)-family' to get the font family name; return nil for emoji icons
(defun tin--icon-family () nil)

(with-eval-after-load 'all-the-icons
  (add-to-list 'all-the-icons-extension-icon-alist '("tin" tin--icon ""))
  (add-to-list 'all-the-icons-mode-icon-alist      '(tin-mode tin--icon "")))

(with-eval-after-load 'nerd-icons
  (add-to-list 'nerd-icons-extension-icon-alist '("tin" tin--icon ""))
  (add-to-list 'nerd-icons-mode-icon-alist      '(tin-mode tin--icon "")))

;;;; Major mode definition

;;;###autoload
(define-derived-mode tin-mode prog-mode "🥫 Tin"
  "Major mode for editing Tin source files.

Based on simpc-mode by rexim (https://github.com/rexim/simpc-mode)."
  :syntax-table tin-mode-syntax-table
  (setq-local font-lock-defaults '(tin-font-lock-keywords))
  (add-hook 'after-change-functions #'tin--invalidate-macro-cache nil t)
  (setq-local comment-start "// ")
  (setq-local comment-end "")
  (setq-local comment-start-skip "//+\\s-*")
  (setq-local indent-line-function #'tin-indent-line)
  (setq-local tab-width 2)
  (setq-local indent-tabs-mode nil))

;;;###autoload
(add-to-list 'auto-mode-alist '("\\.tin\\'" . tin-mode))

(provide 'tin-mode)
;;; tin-mode.el ends here

