<img src="src/main/resources/icons/tin.svg" width="64" alt="Tin icon" align="right"/>

# Tin Language - IntelliJ Plugin

Syntax highlighting and language support for the [Tin programming language](../../README.md) in IntelliJ-based IDEs.

## Features

- Syntax highlighting for `.tin` files
  - Keywords (control flow, declarations, expressions)
  - Built-in types (`i8`, `i64`, `f64`, `bool`, `string`, …)
  - Atom literals (`'ok`, `'err`)
  - String literals with interpolation (`"{expr}"`)
  - Numbers (decimal, hex, binary, octal, float)
  - Control tags (`#inline`, `#export`, …)
  - Operators, brackets, and punctuation
- `//` line comment toggling (`Ctrl+/`)
- Bracket matching for `{}`, `[]`, `()`
- Customizable colors via **Settings -> Editor -> Color Scheme -> Tin**

## Requirements

- IntelliJ-based IDE (IntelliJ IDEA, GoLand, CLion, etc.) build 241-263
- JDK 17+ to build from source

## Building

```sh
# Generate the gradle wrapper (requires Gradle installed)
gradle wrapper

# Build the plugin zip
./gradlew buildPlugin
```

The output zip is at `build/distributions/tin-intellij-<version>.zip`.

## Installing

1. Open your IDE
2. Go to **Settings -> Plugins -> ⚙ -> Install Plugin from Disk…**
3. Select `build/distributions/tin-intellij-<version>.zip`
4. Restart the IDE
