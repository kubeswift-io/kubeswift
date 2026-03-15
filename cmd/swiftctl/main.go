package main

func main() {
	if err := rootCmd.Execute(); err != nil {
		exitErr(err)
	}
}
