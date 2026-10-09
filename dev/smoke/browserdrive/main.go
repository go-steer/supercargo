// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command browserdrive drives the demo's browser UIs in headless Chrome,
// for dev/smoke/browser: Cymbal Shoes books through its assistant and the
// confirmation dialog, Ocean Blue disrupts the voyage from its carrier
// view, and the delay reaches Cymbal's UI live (the alert, the banner,
// the DELAYED card). It's its own module so the SDK's go.mod never
// carries a browser driver.
//
//	browserdrive -chrome PATH -shipper URL -operator URL
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

func main() {
	chrome := flag.String("chrome", "", "the Chrome (or headless shell) binary")
	shipper := flag.String("shipper", "", "Cymbal Shoes' UI, e.g. http://cymbal-shoes.localhost:8442/ui/")
	operator := flag.String("operator", "", "Ocean Blue's UI")
	flag.Parse()
	if err := run(*chrome, *shipper, *operator); err != nil {
		fmt.Fprintln(os.Stderr, "browserdrive:", err)
		os.Exit(1)
	}
}

func run(chrome, shipperURL, operatorURL string) error {
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.WindowSize(1500, 920))
	if chrome != "" {
		opts = append(opts, chromedp.ExecPath(chrome))
	}
	if os.Getuid() == 0 {
		opts = append(opts, chromedp.NoSandbox)
	}
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	root, cancel := context.WithTimeout(alloc, 3*time.Minute)
	defer cancel()

	// Two tabs, one per company: each signs in to its own node.
	cy, cancelCy := chromedp.NewContext(root)
	defer cancelCy()
	ob, cancelOb := chromedp.NewContext(cy) // same browser, another tab
	defer cancelOb()
	// Open both tabs now: a tab lives as long as the context of its first
	// Run, and each step below runs on a shorter-lived one.
	if err := chromedp.Run(cy); err != nil {
		return fmt.Errorf("starting Chrome: %w", err)
	}
	if err := chromedp.Run(ob); err != nil {
		return fmt.Errorf("opening a second tab: %w", err)
	}

	step := func(name string, ctx context.Context, actions ...chromedp.Action) error {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := chromedp.Run(sctx, actions...); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Println("✓", name)
		return nil
	}
	signIn := func(url string) chromedp.Tasks {
		return chromedp.Tasks{
			chromedp.Navigate(url),
			chromedp.WaitVisible(`.persona`, chromedp.ByQuery),
			chromedp.Click(`.persona`, chromedp.ByQuery),
			chromedp.WaitVisible(`#roles .tab`, chromedp.ByQuery),
		}
	}
	ask := func(text string) chromedp.Tasks {
		return chromedp.Tasks{
			chromedp.WaitEnabled(`.composer textarea`, chromedp.ByQuery),
			chromedp.SetValue(`.composer textarea`, text, chromedp.ByQuery),
			chromedp.Click(`.composer button`, chromedp.ByQuery),
		}
	}

	if err := step("Cymbal Shoes: sign in as Alex", cy, signIn(shipperURL), chromedp.WaitVisible(`.composer textarea`, chromedp.ByQuery)); err != nil {
		return err
	}
	if err := step("Cymbal Shoes: ask about the carrier", cy, ask("We have a new carrier, ocean-blue.example. What can they do?"),
		waitText(`.turn.ai`, "ob-demo-1")); err != nil {
		return err
	}
	if err := step("Cymbal Shoes: the assistant drafts; the dialog opens, not yet armed", cy,
		ask("Book one 40ft high-cube of sneakers for CY-PO-55120, Rotterdam to Newark, departing 2026-10-06, with ocean-blue.example. "+
			"Consignee is Cymbal Footwear in Newark (cymbal-footwear.example). It has to arrive before Thursday the 15th."),
		chromedp.WaitVisible(`.dialog`, chromedp.ByQuery),
		waitText(`.dialog`, "Not sent:"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var disabled bool
			if err := chromedp.Evaluate(`document.querySelector('.dialog button.primary').disabled`, &disabled).Do(ctx); err != nil {
				return err
			}
			if !disabled {
				return errors.New("the confirm button was armed the moment the dialog opened")
			}
			return nil
		})); err != nil {
		return err
	}
	if err := step("Cymbal Shoes: a person confirms; the booking is signed and confirmed", cy,
		chromedp.WaitEnabled(`.dialog button.primary`, chromedp.ByQuery),
		chromedp.Click(`.dialog button.primary`, chromedp.ByQuery),
		chromedp.WaitNotPresent(`.dialog`, chromedp.ByQuery),
		waitText(`.chat`, "Booked."),
		waitText(`#messages`, "ocean.booking.confirmation")); err != nil {
		return err
	}
	if err := step("Ocean Blue: sign in as Sam; the booking is there", ob, signIn(operatorURL),
		waitText(`#left`, "OB-BKG-")); err != nil {
		return err
	}
	if err := step("Cymbal Shoes: the vessel departs (a live event)", cy, waitText(`#messages`, "TRANSPORT DEPA ACT")); err != nil {
		return err
	}
	// The delay arrives as two messages milliseconds apart (the event, then
	// the alert). Cymbal's first fetch of its messages after now is held,
	// then answered as if it was taken before both: the page must fetch
	// again for what arrived meanwhile, not drop it.
	time.Sleep(time.Second) // what the departure brought, settled
	if err := holdFirstFetch(cy, "*/api/messages*", 1500*time.Millisecond, `{"messages":[]}`); err != nil {
		return fmt.Errorf("holding Cymbal's next messages fetch: %w", err)
	}
	if err := step("Ocean Blue: disrupt the voyage, confirmed in its dialog", ob,
		chromedp.Click(`.booking .actions button`, chromedp.ByQuery),
		chromedp.WaitVisible(`.dialog`, chromedp.ByQuery),
		chromedp.WaitEnabled(`.dialog button.primary`, chromedp.ByQuery),
		chromedp.Click(`.dialog button.primary`, chromedp.ByQuery),
		chromedp.WaitNotPresent(`.dialog`, chromedp.ByQuery)); err != nil {
		return err
	}
	return step("Cymbal Shoes: the delay arrives live (alert, banner, DELAYED card)", cy,
		chromedp.WaitVisible(`.chip.fam-alert.high`, chromedp.ByQuery),
		chromedp.WaitVisible(`#banner:not([hidden])`, chromedp.ByQuery),
		waitText(`#banner`, "misses your deadline"),
		chromedp.Click(`//button[contains(@class,"tab") and text()="Shipments"]`, chromedp.BySearch),
		chromedp.WaitVisible(`.booking.delayed .delayed-badge`, chromedp.ByQuery))
}

// holdFirstFetch intercepts the tab's requests matching pattern: the first
// is held for hold, then answered with body (a stale response, as if the
// server had answered before anything newer arrived); the rest go through.
func holdFirstFetch(tab context.Context, pattern string, hold time.Duration, body string) error {
	var held atomic.Bool
	c := chromedp.FromContext(tab)
	exec := cdp.WithExecutor(tab, c.Target)
	chromedp.ListenTarget(tab, func(ev any) {
		e, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		go func() {
			if held.CompareAndSwap(false, true) {
				time.Sleep(hold)
				if err := fetch.FulfillRequest(e.RequestID, 200).
					WithResponseHeaders([]*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/json"}}).
					WithBody(base64.StdEncoding.EncodeToString([]byte(body))).Do(exec); err != nil {
					fmt.Fprintln(os.Stderr, "browserdrive: releasing the held fetch:", err)
				}
				return
			}
			if err := fetch.ContinueRequest(e.RequestID).Do(exec); err != nil {
				fmt.Fprintln(os.Stderr, "browserdrive: letting a fetch through:", err)
			}
		}()
	})
	return chromedp.Run(tab, fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: pattern, RequestStage: fetch.RequestStageRequest}}))
}

// waitText waits until the element sel's text contains want.
func waitText(sel, want string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for {
			var text string
			err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(document.querySelector(%q)||{}).innerText||""`, sel), &text))
			if err == nil && strings.Contains(text, want) {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s never showed %q", sel, want)
			case <-time.After(200 * time.Millisecond):
			}
		}
	})
}
