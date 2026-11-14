package fixture

import "fmt"

// ExampleNewWidget renders a widget and asserts its output.
func ExampleNewWidget() {
	fmt.Println(NewWidget("alpha").Render())
	// Output: alpha
}

// Example is the package-level example.
func Example() {
	fmt.Println(Answer)
	// Output: 42
}
