package parser

import (
	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

func (p *Parser) parseSpawnExprStmt() (ast.Node, error) {
	expr, err := p.parseSpawnExpr()
	if err != nil {
		return nil, err
	}

	return &ast.ExprStmt{Expr: expr}, nil
}

// parseSpawnExpr parses spawn as an expression.
func (p *Parser) parseSpawnExpr() (*ast.SpawnExpr, error) {
	p.advance() // consume spawn

	// spawn do: block
	if p.check(lexer.KW_DO) {
		p.advance() // consume do

		if _, err := p.expect(lexer.COLON); err != nil {
			return nil, err
		}

		var (
			block *ast.Block
			err   error
		)

		if p.check(lexer.NEWLINE) {
			p.advance()

			if p.check(lexer.INDENT) {
				// parseBlock consumes INDENT itself; do not advance here.
				block, err = p.parseBlock()
				if err != nil {
					return nil, err
				}
			}
		} else {
			stmt, err := p.parseStatement()
			if err != nil {
				return nil, err
			}

			block = &ast.Block{Stmts: []ast.Node{stmt}}
		}

		return &ast.SpawnExpr{DoBlock: block}, nil
	}

	// spawn expr
	call, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	return &ast.SpawnExpr{Call: call}, nil
}
