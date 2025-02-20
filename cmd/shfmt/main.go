// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pkg/diff"
	"golang.org/x/crypto/ssh/terminal"

	"mvdan.cc/sh/v3/fileutil"
	"mvdan.cc/sh/v3/syntax"
)

var (
	showVersion = flag.Bool("version", false, "")

	list    = flag.Bool("l", false, "")
	write   = flag.Bool("w", false, "")
	simple  = flag.Bool("s", false, "")
	find    = flag.Bool("f", false, "")
	diffOut = flag.Bool("d", false, "")

	langStr = flag.String("ln", "", "")
	posix   = flag.Bool("p", false, "")

	indent      = flag.Uint("i", 0, "")
	binNext     = flag.Bool("bn", false, "")
	caseIndent  = flag.Bool("ci", false, "")
	spaceRedirs = flag.Bool("sr", false, "")
	keepPadding = flag.Bool("kp", false, "")
	minify      = flag.Bool("mn", false, "")

	toJSON = flag.Bool("tojson", false, "")

	parser            *syntax.Parser
	printer           *syntax.Printer
	readBuf, writeBuf bytes.Buffer

	copyBuf = make([]byte, 32*1024)

	in    io.Reader = os.Stdin
	out   io.Writer = os.Stdout
	color bool

	version = "v3.0.0-alpha2"
)

func main() {
	os.Exit(main1())
}

func main1() int {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: shfmt [flags] [path ...]

If no arguments are given, standard input will be used. If a given path
is a directory, it will be recursively searched for shell files - both
by filename extension and by shebang.

  -version  show version and exit

  -l        list files whose formatting differs from shfmt's
  -w        write result to file instead of stdout
  -d        error with a diff when the formatting differs
  -s        simplify the code

Parser options:

  -ln str   language variant to parse (bash/posix/mksh, default "bash")
  -p        shorthand for -ln=posix

Printer options:

  -i uint   indent: 0 for tabs (default), >0 for number of spaces
  -bn       binary ops like && and | may start a line
  -ci       switch cases will be indented
  -sr       redirect operators will be followed by a space
  -kp       keep column alignment paddings
  -mn       minify program to reduce its size (implies -s)

Utilities:

  -f        recursively find all shell files and print the paths
  -tojson   print syntax tree to stdout as a typed JSON
`)
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if *posix && *langStr != "" {
		fmt.Fprintf(os.Stderr, "-p and -ln=lang cannot coexist\n")
		return 1
	}
	lang := syntax.LangBash
	switch *langStr {
	case "bash", "":
	case "posix":
		lang = syntax.LangPOSIX
	case "mksh":
		lang = syntax.LangMirBSDKorn
	default:
		fmt.Fprintf(os.Stderr, "unknown shell language: %s\n", *langStr)
		return 1
	}
	if *posix {
		lang = syntax.LangPOSIX
	}
	if *minify {
		*simple = true
	}
	parser = syntax.NewParser(syntax.KeepComments(true), syntax.Variant(lang))
	printer = syntax.NewPrinter(
		syntax.Indent(*indent),
		syntax.BinaryNextLine(*binNext),
		syntax.SwitchCaseIndent(*caseIndent),
		syntax.SpaceRedirects(*spaceRedirs),
		syntax.KeepPadding(*keepPadding),
		syntax.Minify(*minify),
	)
	if os.Getenv("FORCE_COLOR") == "true" {
		// Undocumented way to force color; used in the tests.
		color = true
	} else if os.Getenv("TERM") == "dumb" {
		// Equivalent to forcing color to be turned off.
	} else if f, ok := out.(*os.File); ok && terminal.IsTerminal(int(f.Fd())) {
		color = true
	}
	if flag.NArg() == 0 {
		if err := formatStdin(); err != nil {
			if err != errChangedWithDiff {
				fmt.Fprintln(os.Stderr, err)
			}
			return 1
		}
		return 0
	}
	if *toJSON {
		fmt.Fprintln(os.Stderr, "-tojson can only be used with stdin/out")
		return 1
	}
	status := 0
	for _, path := range flag.Args() {
		walk(path, func(err error) {
			if err != errChangedWithDiff {
				fmt.Fprintln(os.Stderr, err)
			}
			status = 1
		})
	}
	return status
}

var errChangedWithDiff = fmt.Errorf("")

func formatStdin() error {
	if *write {
		return fmt.Errorf("-w cannot be used on standard input")
	}
	src, err := ioutil.ReadAll(in)
	if err != nil {
		return err
	}
	return formatBytes(src, "<standard input>")
}

var vcsDir = regexp.MustCompile(`^\.(git|svn|hg)$`)

func walk(path string, onError func(error)) {
	info, err := os.Stat(path)
	if err != nil {
		onError(err)
		return
	}
	if !info.IsDir() {
		if err := formatPath(path, false); err != nil {
			onError(err)
		}
		return
	}
	filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() && vcsDir.MatchString(info.Name()) {
			return filepath.SkipDir
		}
		if err != nil {
			onError(err)
			return nil
		}
		conf := fileutil.CouldBeScript(info)
		if conf == fileutil.ConfNotScript {
			return nil
		}
		err = formatPath(path, conf == fileutil.ConfIfShebang)
		if err != nil && !os.IsNotExist(err) {
			onError(err)
		}
		return nil
	})
}

func formatPath(path string, checkShebang bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	readBuf.Reset()
	if checkShebang {
		n, err := f.Read(copyBuf[:32])
		if err != nil {
			return err
		}
		if !fileutil.HasShebang(copyBuf[:n]) {
			return nil
		}
		readBuf.Write(copyBuf[:n])
	}
	if *find {
		fmt.Fprintln(out, path)
		return nil
	}
	if _, err := io.CopyBuffer(&readBuf, f, copyBuf); err != nil {
		return err
	}
	f.Close()
	return formatBytes(readBuf.Bytes(), path)
}

func formatBytes(src []byte, path string) error {
	prog, err := parser.Parse(bytes.NewReader(src), path)
	if err != nil {
		return err
	}
	if *simple {
		syntax.Simplify(prog)
	}
	if *toJSON {
		// must be standard input; fine to return
		return writeJSON(out, prog, true)
	}
	writeBuf.Reset()
	printer.Print(&writeBuf, prog)
	res := writeBuf.Bytes()
	if !bytes.Equal(src, res) {
		if *list {
			if _, err := fmt.Fprintln(out, path); err != nil {
				return err
			}
		}
		if *write {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			if _, err := f.Write(res); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
		if *diffOut {
			if err := diffBytes(src, res, path); err != nil {
				return fmt.Errorf("computing diff: %s", err)
			}
			return errChangedWithDiff
		}
	}
	if !*list && !*write && !*diffOut {
		if _, err := out.Write(res); err != nil {
			return err
		}
	}
	return nil
}

func diffBytes(b1, b2 []byte, path string) error {
	a := bytes.Split(b1, []byte("\n"))
	b := bytes.Split(b2, []byte("\n"))
	ab := diff.Bytes(a, b)
	e := diff.Myers(context.Background(), ab)
	opts := []diff.WriteOpt{diff.Names(path+".orig", path)}
	if color {
		opts = append(opts, diff.TerminalColor())
	}
	if _, err := e.WriteUnified(out, ab, opts...); err != nil {
		return err
	}
	return nil
}
