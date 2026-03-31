;;; tin-mode.el --- Major mode for the Tin programming language  -*- lexical-binding: t; -*-

;; Based on simpc-mode by rexim (https://github.com/rexim/simpc-mode)

;;; Commentary:

;; A simple major mode for the Tin programming language.
;; Features:
;;   - Syntax highlighting for keywords, types, constants, atoms,
;;     control tags, function/type declarations, macros, namespaces
;;   - Line comments (//)
;;   - Indentation: blocks open with = or : at end of line (2-space)
;;   - auto-mode-alist for *.tin

;;; Code:

(require 'subr-x)

;;;; Syntax table

(defvar tin-mode-syntax-table
  (let ((table (make-syntax-table)))
    ;; Line comments: //
    (modify-syntax-entry ?/ ". 12" table)
    (modify-syntax-entry ?\n ">" table)
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
    (modify-syntax-entry ?* "." table)
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
    "use" "export" "extern" "return" "if" "else" "for" "in"
    "match" "case" "default" "defer" "where" "macro" "static"
    "virtual" "as" "is" "forward" "override" "sizeof" "addr"
    "break" "do" "echo" "test" "typeof" "traitof" "fieldnames"
    "fieldtypes" "fieldtag" "getfield" "setfield" "pass" "isrc"
    "var" "spawn" "await" "yield"))

(defun tin-builtin-types ()
  "Tin built-in primitive types."
  '("i8" "i16" "i32" "i64"
    "u8" "u16" "u32" "u64"
    "f32" "f64"
    "bool" "string" "atom" "void" "any"))

(defun tin-constants ()
  "Tin language constants."
  '("true" "false" "nil"))

;;;; Font-lock

(defun tin-font-lock-keywords ()
  "Font-lock keyword list for `tin-mode'."
  (list
   ;; Control tags: #pure  #no_recurse  #sideffect  #allow_sideffect ...
   `("\\(#[a-zA-Z_][a-zA-Z0-9_]*\\)" . font-lock-preprocessor-face)

   ;; Atom literals: 'ok  'err  'some_atom
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

   ;; Namespace access: module::item  (highlight the namespace part)
   `("\\b\\([a-zA-Z_][a-zA-Z0-9_]*\\)::[a-zA-Z_]"
     (1 font-lock-constant-face))))

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

;;;; Major mode definition

;;;###autoload
(define-derived-mode tin-mode prog-mode "Tin"
  "Major mode for editing Tin source files.

Based on simpc-mode by rexim (https://github.com/rexim/simpc-mode)."
  :syntax-table tin-mode-syntax-table
  (setq-local font-lock-defaults '(tin-font-lock-keywords))
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

