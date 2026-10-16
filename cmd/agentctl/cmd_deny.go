package main

func cmdDeny(c *Context, a []string) error { return decide(c, a, "deny") }
