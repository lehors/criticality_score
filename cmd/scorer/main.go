// Copyright 2022 Criticality Score Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The scorer command is used for calculating the criticality score for signals
// generated by the collect_signals command.
//
// The scoring algorithm is defined by a YAML config file that defines the
// basic algorithm (e.g. "pike") and the fields to include in the score. Each
// field's upper and lower bounds, weight and distribution, and whether
// "smaller is better" can be set in the config.
//
// For example:
//
//	algorithm: pike
//	fields:
//	  legacy.created_since:
//	    weight: 1
//	    upper: 120
//	    distribution: zipfian
//
// The raw signals, along with the score, are returning in the output.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	_ "github.com/ossf/criticality_score/cmd/scorer/algorithm/wam"
	log "github.com/ossf/criticality_score/internal/log"
	"github.com/ossf/criticality_score/internal/outfile"
)

const defaultLogLevel = zapcore.InfoLevel

var (
	configFlag     = flag.String("config", "", "the filename of the config (required)")
	columnNameFlag = flag.String("column", "", "the name of the output column")
	logLevel       = defaultLogLevel
	logEnv         log.Env
)

func init() {
	flag.Var(&logLevel, "log", "set the `level` of logging.")
	flag.TextVar(&logEnv, "log-env", log.DefaultEnv, "set logging `env`.")
	outfile.DefineFlags(flag.CommandLine, "force", "append", "OUT_FILE") // TODO: add the ability to disable "append"
	flag.Usage = func() {
		cmdName := path.Base(os.Args[0])
		w := flag.CommandLine.Output()
		fmt.Fprintf(w, "Usage:\n  %s [FLAGS]... IN_CSV OUT_CSV\n\n", cmdName)
		fmt.Fprintf(w, "Scores collected signal for record in the IN_CSV.\n")
		fmt.Fprintf(w, "IN_CSV must be either a csv file or - to read from stdin.\n")
		fmt.Fprintf(w, "OUT_CSV must be either be a csv file or - to write to stdout.\n")
		fmt.Fprintf(w, "\nFlags:\n")
		flag.PrintDefaults()
	}
}

func generateColumnName() string {
	if *columnNameFlag != "" {
		// If we have the column name, just use it as the name
		return *columnNameFlag
	}
	// Get the name of the config file used, without the path
	f := path.Base(*configFlag)
	ext := path.Ext(f)
	// Strip the extension and convert to lowercase
	f = strings.ToLower(strings.TrimSuffix(f, ext))
	// Change any non-alphanumeric character into an underscore
	f = regexp.MustCompile("[^a-z0-9_]").ReplaceAllString(f, "_")
	// Append "_score" to the end
	return f + "_score"
}

func makeOutHeader(header []string, resultColumn string) ([]string, error) {
	for _, h := range header {
		if h == resultColumn {
			return nil, fmt.Errorf("header already contains field %s", resultColumn)
		}
	}
	return append(header, resultColumn), nil
}

func makeRecord(header, row []string) map[string]float64 {
	record := make(map[string]float64)
	for i, k := range header {
		raw := row[i]
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			// Failed to parse raw into a float, ignore the field
			continue
		}
		record[k] = v
	}
	return record
}

func main() {
	flag.Parse()

	logger, err := log.NewLogger(logEnv, logLevel)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	if flag.NArg() != 2 {
		logger.Error("Must have an input file and an output file specified")
		os.Exit(2)
	}
	inFilename := flag.Args()[0]
	outFilename := flag.Args()[1]

	// Open the in-file for reading
	var r *csv.Reader
	if inFilename == "-" {
		logger.Info("Reading from stdin")
		r = csv.NewReader(os.Stdin)
	} else {
		logger.With(
			zap.String("filename", inFilename),
		).Debug("Reading from file")
		f, err := os.Open(inFilename)
		if err != nil {
			logger.With(
				zap.Error(err),
				zap.String("filename", inFilename),
			).Error("Failed to open input file")
			os.Exit(2)
		}
		defer f.Close()
		r = csv.NewReader(f)
	}

	// Open the out-file for writing
	f, err := outfile.Open(context.Background(), outFilename)
	if err != nil {
		logger.With(
			zap.Error(err),
			zap.String("filename", outFilename),
		).Error("Failed to open file for output")
		os.Exit(2)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	// Prepare the algorithm from the config file
	if *configFlag == "" {
		logger.Error("Must have a config file set")
		os.Exit(2)
	}

	cf, err := os.Open(*configFlag)
	if err != nil {
		logger.With(
			zap.Error(err),
			zap.String("filename", *configFlag),
		).Error("Failed to open config file")
		os.Exit(2)
	}
	c, err := LoadConfig(cf)
	if err != nil {
		logger.With(
			zap.Error(err),
			zap.String("filename", *configFlag),
		).Error("Failed to parse config file")
		os.Exit(2)
	}
	a, err := c.Algorithm()
	if err != nil {
		logger.With(
			zap.Error(err),
			zap.String("algorithm", c.Name),
		).Error("Failed to get the algorithm")
		os.Exit(2)
	}

	inHeader, err := r.Read()
	if err != nil {
		logger.With(
			zap.Error(err),
		).Error("Failed to read CSV header row")
		os.Exit(2)
	}

	// Generate and output the CSV header row
	outHeader, err := makeOutHeader(inHeader, generateColumnName())
	if err != nil {
		logger.With(
			zap.Error(err),
		).Error("Failed to generate output header row")
		os.Exit(2)
	}
	if err := w.Write(outHeader); err != nil {
		logger.With(
			zap.Error(err),
		).Error("Failed to write CSV header row")
		os.Exit(2)
	}

	var pq PriorityQueue
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			logger.With(
				zap.Error(err),
			).Error("Failed to read CSV row")
			os.Exit(2)
		}
		record := makeRecord(inHeader, row)
		score := a.Score(record)
		row = append(row, fmt.Sprintf("%.5f", score))
		pq.PushRow(row, score)
	}

	// Iterate over the pq and send the results to the output csv.
	t := pq.Len()
	for i := 0; i < t; i++ {
		if err := w.Write(pq.PopRow()); err != nil {
			logger.With(
				zap.Error(err),
			).Error("Failed to write CSV header row")
			os.Exit(2)
		}
	}
	// -allow-score-override -- if the output field exists overwrite the existing data
}
