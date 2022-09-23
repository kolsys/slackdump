// Package emoji provides functions to dump the all slack emojis for a workspace.
// It skips the "alias" emojis, so only original an emoji with an original name
// is present. If you need to find the alias - lookup the index.json. The
// directory structure is the following:
//
//	.
//	+- emojis
//	|  +- foo.png
//	|  +- bar.png
//	:  :
//	|  +- baz.png
//	+- index.json
//
// Where index.json contains the emoji index, and *.png files under emojis
// directory are individual emojis.
package emoji

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/rusq/slackdump/v2"
	"github.com/rusq/slackdump/v2/auth"
	"github.com/rusq/slackdump/v2/fsadapter"
	"github.com/rusq/slackdump/v2/internal/app/config"
	"github.com/rusq/slackdump/v2/logger"
)

const (
	numWorkers = 12       // default number of download workers.
	emojiDir   = "emojis" // directory where all emojis are downloaded.
)

// Download saves all emojis to "emoji" subdirectory of the Output.Base directory
// or archive.
func Download(ctx context.Context, cfg config.Params, prov auth.Provider) error {
	sess, err := slackdump.NewWithOptions(ctx, prov, cfg.Options)
	if err != nil {
		return err
	}
	fsa, err := fsadapter.ForFilename(cfg.Output.Base)
	if err != nil {
		return fmt.Errorf("unable to initialise adapter for %s: %w", cfg.Output.Base, err)
	}
	defer fsadapter.Close(fsa)

	emojis, err := sess.DumpEmojis(ctx)
	if err != nil {
		return fmt.Errorf("error during emoji dump: %w", err)
	}
	bIndex, err := json.Marshal(emojis)
	if err != nil {
		return fmt.Errorf("error marshalling emoji index: %w", err)
	}
	if err := fsa.WriteFile("index.json", bIndex, 0644); err != nil {
		return fmt.Errorf("failed writing emoji index: %w", err)
	}

	return fetch(ctx, fsa, emojis, fetchEmoji)
}

// fetch downloads the emojis and saves them to the fsa. It spawns numWorker
// goroutines for getting the files. It will call fetchFn for each emoji.
func fetch(ctx context.Context, fsa fsadapter.FS, emojis map[string]string, fetchFn fetchFunc) error {
	var (
		emojiC  = make(chan emoji)
		resultC = make(chan result)
	)

	// Async download pipeline.

	// 1. generator, send emojis into the emojiC channel.
	go func() {
		defer close(emojiC)
		for name, uri := range emojis {
			select {
			case <-ctx.Done():
				return
			case emojiC <- emoji{name, uri}:
			}
		}
	}()

	// 2. Download workers, download the emojis.
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			worker(ctx, fsa, emojiC, resultC, fetchFn)
			wg.Done()
		}()
	}
	// 3. Sentinel, closes the result channel once all workers are finished.
	go func() {
		wg.Wait()
		close(resultC)
	}()

	// 4. Result processor, receives download results and logs any errors that
	//    may have occurred.
	var (
		total = len(emojis)
		count = 0
	)
	for res := range resultC {
		if res.err != nil {
			if errors.Is(res.err, context.Canceled) {
				return res.err
			}
			logger.Default.Printf("failed: %q: %s", res.name, res.err)
			continue
		}
		count++
		logger.Default.Printf("downloaded % 5d/%d %q", count, total, res.name)
	}

	return nil
}

// emoji is an array containing name and url of the emoji.
type emoji [2]string

type result struct {
	name string
	err  error
}

type fetchFunc func(ctx context.Context, fsa fsadapter.FS, dir string, name string, uri string) error

// worker is the function that runs in a separate goroutine and downloads emoji
// received from emojiC. The result of the operation is sent to resultC channel.
// fn is called for each received emoji.
func worker(ctx context.Context, fsa fsadapter.FS, emojiC <-chan emoji, resultC chan<- result, fetchFn fetchFunc) {
	for {
		select {
		case <-ctx.Done():
			resultC <- result{err: ctx.Err()}
			return
		case emoji, more := <-emojiC:
			if !more {
				return
			}
			if strings.HasPrefix(emoji[1], "alias:") {
				resultC <- result{name: emoji[0] + "(alias, skipped)"}
				break
			}
			err := fetchFn(ctx, fsa, emojiDir, emoji[0], emoji[1])
			resultC <- result{name: emoji[0], err: err}
		}
	}
}

// fetchEmoji downloads one emoji file from uri into the filename dir/name.png
// within the filesystem adapter fsa.
func fetchEmoji(ctx context.Context, fsa fsadapter.FS, dir string, name, uri string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	filename := path.Join(dir, name+".png")
	wc, err := fsa.Create(filename)
	if err != nil {
		return err
	}
	defer wc.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid server status code: %d (%s)", resp.StatusCode, resp.Status)
	}

	if _, err := io.Copy(wc, resp.Body); err != nil {
		return err
	}

	return nil
}