# Tin Language - VS Code Extension

Syntax highlighting for the Tin programming language.

## Features

- Keywords, types, strings, atoms, numbers, booleans, control tags
- String interpolation highlighted as embedded expressions
- Macro declarations and call-sites
- Declaration name highlighting (struct, trait, enum, union, type)
- Namespace/module path highlighting
- Operator and punctuation highlighting
- Line comments, bracket-matching, auto-closing pairs
- Indent-on-= (tin uses = to open blocks)

## Installation

Sideload: symlink into VS Code extensions, then reload.

    ln -s /path/to/tin/utils/tin-vscode ~/.vscode/extensions/tin-language

Or build a .vsix:

    cd utils/tin-vscode && npm install -g @vscode/vsce && vsce package
    code --install-extension tin-language-0.1.0.vsix
