package lsp

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"

	"github.com/wader/gojq"
)

//go:embed builtin_env.jq
//go:embed docs.jq
//go:embed lsp.jq
var lspFS embed.FS

type loadModule struct {
	init func() ([]*gojq.Query, error)
	load func(name string) (*gojq.Query, error)
}

func (l loadModule) LoadInitModules() ([]*gojq.Query, error)     { return l.init() }
func (l loadModule) LoadModule(name string) (*gojq.Query, error) { return l.load(name) }

type interp struct {
	readFileFn func(string) ([]byte, error)
	stdinR     io.Reader
	stdoutW    io.Writer
	stderrW    io.Writer
	environ    []string
}

type parseError struct {
	err    error
	offset int
}

func (ce parseError) Value() interface{} {
	return map[string]interface{}{
		"error":  ce.err.Error(),
		"offset": ce.offset,
	}
}

func (ee parseError) Error() string {
	return fmt.Sprintf("%d: %s", ee.offset, ee.err.Error())
}

func queryErrorPosition(v error) int {
	var offset int
	if tokIf, ok := v.(interface{ Token() (string, int) }); ok { //nolint:errorlint
		_, offset = tokIf.Token()
	}
	return offset
}

func Run(readFileFn func(string) ([]byte, error), stdin io.Reader, stdout io.Writer, stderr io.Writer, environ []string) error {
	i := &interp{
		readFileFn: readFileFn,
		stdinR:     stdin,
		stdoutW:    stdout,
		stderrW:    stderr,
		environ:    environ,
	}

	var state interface{}

	// TODO: currently the main serve loop is done in go until
	// https://github.com/itchyny/gojq/issues/86 has been resolved
	// TODO: could probably reuse *gojq.Code instance
	for {
		iter, err := i.Eval("serve", state)
		if err != nil {
			return err
		}
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}

			switch v := v.(type) {
			case error:
				fmt.Fprintln(stderr, v)
				return v
			case [2]interface{}:
				fmt.Fprintln(stderr, v[:]...)
			default:
				state = v
			}
		}
	}
}

func (i *interp) Eval(src string, c interface{}) (gojq.Iter, error) {
	gq, err := gojq.Parse(src)
	if err != nil {
		return nil, err
	}

	var compilerOpts []gojq.CompilerOption
	compilerOpts = append(compilerOpts, gojq.WithEnvironLoader(func() []string { return i.environ }))
	compilerOpts = append(compilerOpts, gojq.WithModuleLoader(loadModule{
		init: func() ([]*gojq.Query, error) {
			gq, err := gojq.Parse(`include "lsp";`)
			if err != nil {
				return nil, err
			}
			return []*gojq.Query{gq}, nil
		},
		load: func(name string) (*gojq.Query, error) {
			f, err := lspFS.Open(name + ".jq")
			if err != nil {
				return nil, err
			}
			defer f.Close()
			b, err := io.ReadAll(f)
			if err != nil {
				return nil, err
			}
			gq, err := gojq.Parse(string(b))
			if err != nil {
				return nil, err
			}
			return gq, nil
		},
	}))

	compilerOpts = append(compilerOpts, gojq.WithFunction("readfile", 0, 0, i.readFile))
	compilerOpts = append(compilerOpts, gojq.WithFunction("stdin", 0, 1, i.stdin))
	compilerOpts = append(compilerOpts, gojq.WithIterFunction("stdout", 0, 0, i.stdout))
	compilerOpts = append(compilerOpts, gojq.WithIterFunction("stderr", 0, 0, i.stderr))
	compilerOpts = append(compilerOpts, gojq.WithFunction("query_fromstring", 0, 0, i.queryFromString))
	compilerOpts = append(compilerOpts, gojq.WithFunction("query_tostring", 0, 0, i.queryToString))

	gc, err := gojq.Compile(gq, compilerOpts...)
	if err != nil {
		return nil, err
	}

	return gc.RunWithContext(context.Background(), c), nil
}

func (i *interp) readFile(c interface{}, a []interface{}) interface{} {
	path, err := toString(c)
	if err != nil {
		return err
	}
	b, err := i.readFileFn(path)
	if err != nil {
		return err
	}
	return string(b)
}

func (i *interp) stdin(_ interface{}, a []interface{}) interface{} {
	var n int
	if len(a) >= 1 {
		var err error
		n, err = toInt(a[0])
		if err != nil {
			return err
		}
	}

	if n == 0 {
		b := &bytes.Buffer{}
		if _, err := io.Copy(b, i.stdinR); err != nil {
			return err
		}
		return b.String()
	}
	b := make([]byte, n)

	_, err := io.ReadFull(i.stdinR, b)
	if err != nil {
		return err
	}

	return string(b)
}

func (i *interp) stdout(c interface{}, a []interface{}) gojq.Iter {
	if _, err := fmt.Fprint(i.stdoutW, c); err != nil {
		return gojq.NewIter(err)
	}
	return gojq.NewIter()
}

func (i *interp) stderr(c interface{}, a []interface{}) gojq.Iter {
	if _, err := fmt.Fprint(i.stderrW, c); err != nil {
		return gojq.NewIter(err)
	}
	return gojq.NewIter()
}

func (i *interp) queryFromString(c interface{}, a []interface{}) interface{} {
	s, err := toString(c)
	if err != nil {
		return err
	}
	q, err := gojq.Parse(s)
	if err != nil {
		offset := queryErrorPosition(err)
		return parseError{
			err:    err,
			offset: offset,
		}
	}

	b, err := json.Marshal(q)
	if err != nil {
		return err
	}

	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}

	return v
}

func (i *interp) queryToString(c interface{}, a []interface{}) interface{} {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	var q gojq.Query
	if err := json.Unmarshal(b, &q); err != nil {
		return err
	}

	return q.String()
}

func toString(v interface{}) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("value can't be a string")
	}
}

func toInt(v interface{}) (int, error) {
	// TODO: other types
	switch v := v.(type) {
	case int:
		return v, nil
	default:
		return 0, fmt.Errorf("value can't be a int")
	}
}
