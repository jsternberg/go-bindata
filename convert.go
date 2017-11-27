// This work is subject to the CC0 1.0 Universal (CC0 1.0) Public Domain Dedication
// license. Its contents can be found at:
// http://creativecommons.org/publicdomain/zero/1.0/

package bindata

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Translate reads assets from an input directory, converts them
// to Go code and writes new files to the output specified
// in the given configuration.
func Translate(c *Config) error {
	var toc []Asset

	// Ensure our configuration has sane values.
	err := c.validate()
	if err != nil {
		return err
	}

	var knownFuncs = make(map[string]int)
	var visitedPaths = make(map[string]bool)
	// Locate all the assets.
	for _, input := range c.Input {
		err = findFiles(input.Path, c.Prefix, input.Recursive, &toc, c.Ignore, knownFuncs, visitedPaths)
		if err != nil {
			return err
		}
	}

	// Create output file.
	buf := new(bytes.Buffer)
	// Write the header. This makes e.g. Github ignore diffs in generated files.
	if _, err = fmt.Fprint(buf, "// Code generated by go-bindata. DO NOT EDIT.\n"); err != nil {
		return err
	}
	if _, err = fmt.Fprint(buf, "// sources:\n"); err != nil {
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	for _, asset := range toc {
		relative, _ := filepath.Rel(wd, asset.Path)
		if _, err = fmt.Fprintf(buf, "// %s\n", filepath.ToSlash(relative)); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprint(buf, "\n"); err != nil {
		return err
	}

	// Write build tags, if applicable.
	if len(c.Tags) > 0 {
		if _, err = fmt.Fprintf(buf, "// +build %s\n\n", c.Tags); err != nil {
			return err
		}
	}

	// Write package declaration.
	_, err = fmt.Fprintf(buf, "package %s\n\n", c.Package)
	if err != nil {
		return err
	}

	// Write assets.
	if c.Debug || c.Dev {
		if os.Getenv("GO_BINDATA_TEST") == "true" {
			// If we don't do this, people running the tests on different
			// machines get different git diffs.
			for i := range toc {
				toc[i].Path = strings.Replace(toc[i].Path, wd, "/test", 1)
			}
		}
		err = writeDebug(buf, c, toc)
	} else {
		err = writeRelease(buf, c, toc)
	}

	if err != nil {
		return err
	}

	// Write table of contents
	if err := writeTOC(buf, toc); err != nil {
		return err
	}
	// Write hierarchical tree of assets
	if err := writeTOCTree(buf, toc); err != nil {
		return err
	}

	// Write restore procedure
	if err := writeRestore(buf); err != nil {
		return err
	}
	fmted, err := format.Source(buf.Bytes())
	if err != nil {
		return err
	}

	return safefileWriteFile(c.Output, fmted, 0666)
}

// Implement sort.Interface for []os.FileInfo based on Name()
type byName []os.FileInfo

func (v byName) Len() int           { return len(v) }
func (v byName) Swap(i, j int)      { v[i], v[j] = v[j], v[i] }
func (v byName) Less(i, j int) bool { return v[i].Name() < v[j].Name() }

// findFiles recursively finds all the file paths in the given directory tree.
// They are added to the given map as keys. Values will be safe function names
// for each file, which will be used when generating the output code.
func findFiles(dir, prefix string, recursive bool, toc *[]Asset, ignore []*regexp.Regexp, knownFuncs map[string]int, visitedPaths map[string]bool) error {
	dirpath := dir
	if len(prefix) > 0 {
		dirpath, _ = filepath.Abs(dirpath)
		prefix, _ = filepath.Abs(prefix)
		prefix = filepath.ToSlash(prefix)
	}

	fi, err := os.Stat(dirpath)
	if err != nil {
		return err
	}

	var list []os.FileInfo

	if !fi.IsDir() {
		dirpath = filepath.Dir(dirpath)
		list = []os.FileInfo{fi}
	} else {
		visitedPaths[dirpath] = true
		fd, err := os.Open(dirpath)
		if err != nil {
			return err
		}

		defer fd.Close()

		list, err = fd.Readdir(0)
		if err != nil {
			return err
		}

		// Sort to make output stable between invocations
		sort.Sort(byName(list))
	}

	for _, file := range list {
		var asset Asset
		asset.Path = filepath.Join(dirpath, file.Name())
		asset.Name = filepath.ToSlash(asset.Path)

		ignoring := false
		for _, re := range ignore {
			if re.MatchString(asset.Path) {
				ignoring = true
				break
			}
		}
		if ignoring {
			continue
		}

		if file.IsDir() {
			if recursive {
				recursivePath := filepath.Join(dir, file.Name())
				visitedPaths[asset.Path] = true
				findFiles(recursivePath, prefix, recursive, toc, ignore, knownFuncs, visitedPaths)
			}
			continue
		} else if file.Mode()&os.ModeSymlink == os.ModeSymlink {
			var linkPath string
			if linkPath, err = os.Readlink(asset.Path); err != nil {
				return err
			}
			if !filepath.IsAbs(linkPath) {
				if linkPath, err = filepath.Abs(dirpath + "/" + linkPath); err != nil {
					return err
				}
			}
			if _, ok := visitedPaths[linkPath]; !ok {
				visitedPaths[linkPath] = true
				findFiles(asset.Path, prefix, recursive, toc, ignore, knownFuncs, visitedPaths)
			}
			continue
		}

		if strings.HasPrefix(asset.Name, prefix) {
			asset.Name = asset.Name[len(prefix):]
		} else {
			asset.Name = filepath.Join(dir, file.Name())
		}

		// If we have a leading slash, get rid of it.
		if len(asset.Name) > 0 && asset.Name[0] == '/' {
			asset.Name = asset.Name[1:]
		}

		// This shouldn't happen.
		if len(asset.Name) == 0 {
			return fmt.Errorf("Invalid file: %v", asset.Path)
		}

		asset.Func = safeFunctionName(asset.Name, knownFuncs)
		asset.Path, err = filepath.Abs(asset.Path)
		if err != nil {
			return err
		}
		*toc = append(*toc, asset)
	}

	return nil
}

var regFuncName = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// safeFunctionName converts the given name into a name
// which qualifies as a valid function identifier. It
// also compares against a known list of functions to
// prevent conflict based on name translation.
func safeFunctionName(name string, knownFuncs map[string]int) string {
	var inBytes, outBytes []byte
	var toUpper bool

	name = strings.ToLower(name)
	inBytes = []byte(name)

	for i := 0; i < len(inBytes); i++ {
		if regFuncName.Match([]byte{inBytes[i]}) {
			toUpper = true
		} else if toUpper {
			outBytes = append(outBytes, []byte(strings.ToUpper(string(inBytes[i])))...)
			toUpper = false
		} else {
			outBytes = append(outBytes, inBytes[i])
		}
	}

	name = string(outBytes)

	// Identifier can't start with a digit.
	if unicode.IsDigit(rune(name[0])) {
		name = "_" + name
	}

	if num, ok := knownFuncs[name]; ok {
		knownFuncs[name] = num + 1
		name = fmt.Sprintf("%s%d", name, num)
	} else {
		knownFuncs[name] = 2
	}

	return name
}
