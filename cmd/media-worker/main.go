// Command media-worker is the stock media worker (media/worker.Main), which
// registers no master hooks. A host whose kinds' recipes name hooks builds
// its own binary the same way, passing them to worker.Main.
package main

import "github.com/open-rails/contentkit/media/worker"

func main() { worker.Main() }
