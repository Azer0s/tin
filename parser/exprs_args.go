package parser

import (
	"math/big"
	"strconv"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

func (p *Parser) parseArgList() ([]ast.Node, error) {
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}

	p.skipWhitespace()

	var args []ast.Node

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		args = append(args, arg)

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
			p.skipWhitespace()
		}
	}

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}

	return args, nil
}

// parseIntLitToken converts the textual form of an integer literal into an
// ast.IntLit. Two encodings coexist:
//
//   - Within u64 range: stored in IntLit.Value as the i64 bit pattern. This
//     preserves existing behavior for hex constants like 0xffffffffffffffff
//     (-1 as i64, 18446744073709551615 as u64) where the variable's declared
//     type decides signedness.
//   - Above u64 range: IntLit.Big is set to the exact magnitude, and Value
//     keeps the bottom 64 bits as a fallback. Codegen reads Big to emit an
//     i128 constant (auto-upgrade); paths that ignore Big see the truncated
//     bottom 64 bits, matching the behavior of explicit truncation.
func parseIntLitToken(lit string) *ast.IntLit {
	if v, err := strconv.ParseInt(lit, 0, 64); err == nil {
		return &ast.IntLit{Value: v}
	}

	if uv, err := strconv.ParseUint(lit, 0, 64); err == nil {
		return &ast.IntLit{Value: int64(uv)}
	}

	// Exceeds u64. Parse as big.Int (handles 0x prefix) and stash both the
	// big magnitude and a bit-truncated i64 view.
	base := 10

	s := lit
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		base = 16
		s = s[2:]
	} else if strings.HasPrefix(s, "0b") || strings.HasPrefix(s, "0B") {
		base = 2
		s = s[2:]
	} else if strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O") {
		base = 8
		s = s[2:]
	}

	bigVal, ok := new(big.Int).SetString(s, base)
	if !ok {
		// Lexer should have rejected malformed digits already; fall back to
		// zero rather than panicking in the And() below.
		return &ast.IntLit{Value: 0}
	}

	low := new(big.Int).And(bigVal, mask64).Int64()

	return &ast.IntLit{Value: low, Big: bigVal}
}

var mask64 = new(big.Int).SetUint64(^uint64(0))
