// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/magefile/mage/sh"
	"github.com/pkg/errors"

	"github.com/elastic/package-registry/util"
)

var (
	tarGz bool
	copy  bool
)

const (
	packageDirName = "package"
	streamFields   = `
- name: stream.type
  type: constant_keyword
  description: >
    Stream type
- name: stream.dataset
  type: constant_keyword
  description: >
    Stream dataset.
- name: stream.namespace
  type: constant_keyword
  description: >
    Stream namespace.
- name: "@timestamp"
  type: date
  description: >
    Event timestamp.
`
)

func main() {
	// Directory with a list of packages inside
	var sourceDir string
	// Target public directory where the generated packages should end up in
	var publicDir string

	flag.StringVar(&sourceDir, "sourceDir", "", "Path to the source packages")
	flag.StringVar(&publicDir, "publicDir", "", "Path to the public directory ")
	flag.BoolVar(&copy, "copy", true, "If packages should be copied over")
	flag.BoolVar(&tarGz, "tarGz", true, "If packages should be tar gz")
	flag.Parse()

	if sourceDir == "" || publicDir == "" {
		log.Fatal("sourceDir and publicDir must be set")
	}

	if err := Build(sourceDir, publicDir); err != nil {
		log.Fatal(err)
	}
}

func Build(sourceDir, publicDir string) error {
	err := BuildPackages(sourceDir, filepath.Join(publicDir, packageDirName))
	if err != nil {
		return err
	}
	return nil
}

// CopyPackage copies the files of a package to the public directory
func CopyPackage(src, dst string) error {
	log.Println(">> Copy package: " + src)
	err := os.MkdirAll(dst, 0755)
	if err != nil {
		return err
	}
	err = sh.RunV("rsync", "-a", src, dst)
	if err != nil {
		return err
	}

	return nil
}

// BuildPackage rebuilds the zip files inside packages
func BuildPackages(sourceDir, packagesPath string) error {
	list, err := filepath.Glob(sourceDir + "/*/*")
	if err != nil {
		return err
	}

	for _, p := range list {
		fileInfo, err := os.Stat(p)
		if err != nil {
			return err
		}

		// Skip all non directories
		if !fileInfo.IsDir() {
			continue
		}

		packageVersion := filepath.Base(p)
		packageName := filepath.Base(filepath.Dir(p))
		packagePath := filepath.Join(packageName, packageVersion)
		dstDir := filepath.Join(packagesPath, packagePath)

		if copy {
			// Trailing slash is to make sure content of package is copied
			err := CopyPackage(filepath.Join(sourceDir, packagePath)+"/", dstDir)
			if err != nil {
				return err
			}
		}

		p, err := util.NewPackage(dstDir)
		if err != nil {
			return err
		}

		err = buildPackage(packagesPath, *p)
		if err != nil {
			return err
		}
	}
	return nil
}

func buildPackage(packagesBasePath string, p util.Package) error {
	// Change path to simplify tar command
	currentPath, err := os.Getwd()
	if err != nil {
		return err
	}

	// Checks if the package is valid
	err = p.Validate()
	if err != nil {
		return errors.Wrapf(err, "package validation failed (path: %s", p.GetPath())
	}

	p.BasePath = filepath.Join(currentPath, packagesBasePath, p.GetPath())

	datasets, err := p.GetDatasetPaths()
	if err != nil {
		return err
	}

	// Add base-fields.yml to all dataset with the basic stream fields and @timestamp
	for _, dataset := range datasets {
		dirPath := filepath.Join(p.BasePath, "dataset", dataset, "fields")
		err := os.MkdirAll(dirPath, 0755)
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(filepath.Join(dirPath, "base-fields.yml"), []byte(streamFields), 0644)
		if err != nil {
			return err
		}
	}

	err = p.LoadAssets(p.GetPath())
	if err != nil {
		return err
	}

	err = p.LoadDataSets(p.GetPath())
	if err != nil {
		return err
	}

	err = writeJsonFile(p, filepath.Join(packagesBasePath, p.GetPath(), "index.json"))
	if err != nil {
		return err
	}

	// Get all Kibana files
	savedObjects1, err := filepath.Glob(filepath.Join(packagesBasePath, p.GetPath(), "dataset", "*", "kibana", "*", "*"))
	if err != nil {
		return err
	}
	savedObjects2, err := filepath.Glob(filepath.Join(packagesBasePath, p.GetPath(), "kibana", "*", "*"))
	if err != nil {
		return err
	}
	savedObjects := append(savedObjects1, savedObjects2...)

	// Run each file through the saved object encoder
	for _, file := range savedObjects {

		data, err := ioutil.ReadFile(file)
		if err != nil {
			return err
		}
		output, err := encodedSavedObject(data)
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(file, []byte(output), 0644)
		if err != nil {
			return err
		}
	}
	return nil
}

func writeJsonFile(v interface{}, path string) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(path, data, 0644)
}

var (
	fieldsToEncode = []string{
		"attributes.kibanaSavedObjectMeta.searchSourceJSON",
		"attributes.layerListJSON",
		"attributes.mapStateJSON",
		"attributes.optionsJSON",
		"attributes.panelsJSON",
		"attributes.uiStateJSON",
		"attributes.visState",
	}
)

// encodeSavedObject encodes all the fields inside a saved object
// which are stored in encoded JSON in Kibana.
// The reason is that for versioning it is much nicer to have the full
// json so only on packaging this is changed.
func encodedSavedObject(data []byte) (string, error) {
	savedObject := MapStr{}
	err := json.Unmarshal(data, &savedObject)
	if err != nil {
		return "", errors.Wrapf(err, "unmarshalling saved object failed")
	}

	for _, v := range fieldsToEncode {
		out, err := savedObject.GetValue(v)
		// This means the key did not exists, no conversion needed
		if err != nil {
			continue
		}

		// It may happen that some objects existing in example directory might be already encoded.
		// In this case skip the encoding.
		_, isString := out.(string)
		if isString {
			return "", fmt.Errorf("expect non-string field type (fieldName: %s)", v)
		}

		// Marshal the value to encode it properly
		r, err := json.Marshal(&out)
		if err != nil {
			return "", err
		}
		_, err = savedObject.Put(v, string(r))
		if err != nil {
			return "", errors.Wrapf(err, "can't put value to the saved object")
		}

	}

	return savedObject.StringToPrint(), nil
}
