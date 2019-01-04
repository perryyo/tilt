//+build integration

package integration

import (
	"context"
	"testing"
	"time"
)

func TestOneDockerCompose(t *testing.T) {
	f := newFixture(t, "onedc")
	defer f.TearDown()

	f.TiltWatch("web")

	ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
	defer cancel()
	f.CurlUntil(ctx, "http://localhost:31235", "🍄 One-Up! 🍄")

	// TODO(nick): uncomment when file-watching works
	// f.ReplaceContents("main.go", "One-Up", "Two-Up")

	// ctx, cancel = context.WithTimeout(f.ctx, time.Minute)
	// defer cancel()
	// f.CurlUntil(ctx, "http://localhost:31235", "🍄 Two-Up! 🍄")
}
