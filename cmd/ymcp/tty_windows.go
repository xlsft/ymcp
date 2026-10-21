package main

// drainTerminalReplies is a no-op on Windows, where termenv doesn't query
// the terminal at startup.
func drainTerminalReplies() {}
