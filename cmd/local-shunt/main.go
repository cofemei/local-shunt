// Command local-shunt delegates large file reads and boilerplate writes to a worker model.
package main

import "github.com/cofemei/local-shunt/internal/shunt"

func main() {
	shunt.Run()
}
