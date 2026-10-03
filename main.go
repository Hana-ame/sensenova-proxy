package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

// version 必须跟随 tag 一起 bump：v1.3.0 发布时忘了改，导致 `sensenova-proxy -v`
// 打成 v1.2.0，排查时误以为下错了包。
const version = "1.3.4"

func main() {
	configPathFlag := flag.String("config", "", "Path to JSON configuration file (default: config.json)")
	flag.StringVar(configPathFlag, "c", "", "Shorthand for -config")
	showVersion := flag.Bool("v", false, "Print version and exit")
	flag.BoolVar(showVersion, "version", false, "Print version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Printf("sensenova-proxy v%s\n", version)
		os.Exit(0)
	}

	configPath := *configPathFlag
	if configPath == "" {
		if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
			configPath = envPath
		} else if envPath := os.Getenv("CONFIG_FILE"); envPath != "" {
			configPath = envPath
		} else {
			configPath = "config.json"
		}
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("Starting SenseNova Proxy v%s...", version)
	log.Printf("Loading configuration from: %s", configPath)

	items, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Successfully loaded %d proxy configuration(s).", len(items))

	mgr := NewProxyManager(items)
	if err := mgr.Start(); err != nil {
		log.Fatalf("Proxy manager exited with error: %v", err)
	}
}
