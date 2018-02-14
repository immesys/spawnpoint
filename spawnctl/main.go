package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	yaml "gopkg.in/yaml.v2"

	"github.com/SoftwareDefinedBuildings/spawnpoint/service"
	"github.com/SoftwareDefinedBuildings/spawnpoint/spawnclient"
	"github.com/SoftwareDefinedBuildings/spawnpoint/spawnd/util"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

func main() {
	app := cli.NewApp()
	app.Name = "spawnctl"
	app.Usage = "Interact with Spawnpoint daemons"
	app.Version = util.VersionNum

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "router, r",
			Usage:  "Set the BW2 agent to use",
			EnvVar: "BW2_AGENT",
		},
		cli.StringFlag{
			Name:   "entity, e",
			Usage:  "Set the BW2 entity to use",
			EnvVar: "BW2_DEFAULT_ENTITY",
		},
	}

	app.Commands = []cli.Command{
		{
			Name:   "deploy",
			Usage:  "Deploy a configuration to a Spawnpoint daemon",
			Action: actionDeploy,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:   "uri, u",
					Usage:  "BW2 URI of the destination Spawnpoint",
					Value:  "",
					EnvVar: "SPAWNPOINT_DEFAULT_URI",
				},
				cli.StringFlag{
					Name:  "configuration, c",
					Usage: "The configuration to deploy",
					Value: "",
				},
				cli.StringFlag{
					Name:  "name, n",
					Usage: "Name of the service",
					Value: "",
				},
			},
		},
	}

	app.Run(os.Args)
}

func actionDeploy(c *cli.Context) error {
	entity := c.GlobalString("entity")
	if len(entity) == 0 {
		fmt.Println("Missing 'entity' parameter")
		os.Exit(1)
	}

	spawnpointURI := fixURI(c.String("uri"))
	if len(spawnpointURI) == 0 {
		fmt.Println("Missing 'uri' parameter")
		os.Exit(1)
	}

	cfgFile := c.String("configuration")
	if len(cfgFile) == 0 {
		fmt.Println("Missing 'configuration' parameter")
		os.Exit(1)
	}
	config, err := parseSvcConfig(cfgFile)
	if err != nil {
		fmt.Printf("Failed to parse service configuration file: %s\n", err)
		os.Exit(1)
	}

	svcName := c.String("name")
	if len(svcName) == 0 {
		svcName = config.Name
		if len(svcName) == 0 {
			fmt.Println("Missing 'name' parameter or 'Name' field in service configuration")
			os.Exit(1)
		}
	}

	spawnClient, err := spawnclient.New(c.GlobalString("router"), entity)
	if err != nil {
		fmt.Printf("Could not create spawnpoint client: %s\n", err)
	}

	logChan, errChan, doneChan := spawnClient.Tail(svcName, spawnpointURI)
	// Check if an error has already occurred
	select {
	case err = <-errChan:
		fmt.Printf("Could not tail service logs: %s\n", err)
		os.Exit(1)
	default:
	}
	defer close(doneChan)

	if err = spawnClient.Deploy(config, spawnpointURI); err != nil {
		fmt.Printf("Failed to deploy service: %s\n", err)
		os.Exit(1)
	}

	fmt.Println("Tailing service logs. Press CTRL-c to exit...")
	for msg := range logChan {
		fmt.Println(strings.TrimSpace(msg.Contents))
	}
	// Check again if any errors occurred while tailing service log
	select {
	case err = <-errChan:
		fmt.Printf("Error occurred while tailing logs: %s\n", err)
		os.Exit(1)
	default:
	}

	return nil
}

func fixURI(uri string) string {
	if len(uri) > 0 && uri[len(uri)-1] == '/' {
		return uri[:len(uri)-1]
	}
	return uri
}

func parseSvcConfig(configFile string) (*service.Configuration, error) {
	contents, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to read configuration file")
	}

	var svcConfig service.Configuration
	if err = yaml.Unmarshal(contents, &svcConfig); err != nil {
		return nil, errors.Wrap(err, "Failed to parse service configuration")
	}

	return &svcConfig, nil
}
