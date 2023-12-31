package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/rw"
	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
	"google.golang.org/protobuf/proto"

	"github.com/google/go-github/v45/github"
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/inserter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
	"github.com/sirupsen/logrus"
)

var githubClient *github.Client

func init() {
	accessToken, loaded := os.LookupEnv("ACCESS_TOKEN")
	if !loaded {
		githubClient = github.NewClient(nil)
		return
	}
	transport := &github.BasicAuthTransport{
		Username: accessToken,
	}
	githubClient = github.NewClient(transport.Client())
}

func fetch(from string) (*github.RepositoryRelease, error) {
	names := strings.SplitN(from, "/", 2)
	latestRelease, _, err := githubClient.Repositories.GetLatestRelease(context.Background(), names[0], names[1])
	if err != nil {
		return nil, err
	}
	return latestRelease, err
}

func get(downloadURL *string) ([]byte, error) {
	logrus.Info("download ", *downloadURL)
	response, err := http.Get(*downloadURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func download(release *github.RepositoryRelease) ([]byte, error) {
	geoipAsset := common.Find(release.Assets, func(it *github.ReleaseAsset) bool {
		return *it.Name == "geoip.dat"
	})
	if geoipAsset == nil {
		return nil, E.New("geoip.dat not found in upstream release ", release.Name)
	}
	return get(geoipAsset.BrowserDownloadURL)
}

func parse(binary []byte) (countryMap map[string][]*net.IPNet, err error) {
	var geoipList routercommon.GeoIPList
	if err = proto.Unmarshal(binary, &geoipList); err != nil {
		return
	}

	countryMap = make(map[string][]*net.IPNet)
	for _, geoip := range geoipList.Entry {
		code := strings.ToLower(geoip.CountryCode)
		for _, cidr := range geoip.Cidr {
			ip := net.IP(cidr.Ip)
			ipNet := &net.IPNet{
				IP:   ip,
				Mask: net.CIDRMask(int(cidr.Prefix), 8*len(ip)),
			}
			countryMap[code] = append(countryMap[code], ipNet)
		}
	}
	return
}

func newWriter(codes []string) (*mmdbwriter.Tree, error) {
	return mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "sing-geoip",
		Languages:               codes,
		IPVersion:               6,
		RecordSize:              24,
		Inserter:                inserter.ReplaceWith,
		DisableIPv4Aliasing:     true,
		IncludeReservedNetworks: true,
	})
}

func open(path string, codes []string) (*mmdbwriter.Tree, error) {
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	if reader.Metadata.DatabaseType != "sing-geoip" {
		return nil, E.New("invalid sing-geoip database")
	}
	reader.Close()

	return mmdbwriter.Load(path, mmdbwriter.Options{
		Languages: append(reader.Metadata.Languages, common.Filter(codes, func(it string) bool {
			return !common.Contains(reader.Metadata.Languages, it)
		})...),
		Inserter: inserter.ReplaceWith,
	})
}

func write(writer *mmdbwriter.Tree, dataMap map[string][]*net.IPNet, output string, codes []string) error {
	if len(codes) == 0 {
		codes = make([]string, 0, len(dataMap))
		for code := range dataMap {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	codeMap := make(map[string]bool)
	for _, code := range codes {
		codeMap[code] = true
	}
	for code, data := range dataMap {
		if !codeMap[code] {
			continue
		}
		for _, item := range data {
			err := writer.Insert(item, mmdbtype.String(code))
			if err != nil {
				return err
			}
		}
	}
	outputFile, err := os.Create(output)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	_, err = writer.WriteTo(outputFile)
	return err
}

func local(input string, output string, codes []string) error {
	binary, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	countryMap, err := parse(binary)
	if err != nil {
		return err
	}
	var writer *mmdbwriter.Tree
	if rw.FileExists(output) {
		writer, err = open(output, codes)
	} else {
		allCodes := make([]string, 0, len(countryMap))
		for code := range countryMap {
			allCodes = append(allCodes, code)
		}
		writer, err = newWriter(allCodes)
	}
	if err != nil {
		return err
	}
	return write(writer, countryMap, output, codes)
}

func release(source string, destination string) error {
	sourceRelease, err := fetch(source)
	if err != nil {
		return err
	}
	destinationRelease, err := fetch(destination)
	if err != nil {
		logrus.Warn("missing destination latest release")
	} else {
		if os.Getenv("NO_SKIP") != "true" && strings.Contains(*destinationRelease.Name, *sourceRelease.Name) {
			logrus.Info("already latest")
			setActionOutput("skip", "true")
			return nil
		}
	}
	binary, err := download(sourceRelease)
	if err != nil {
		return err
	}
	countryMap, err := parse(binary)
	if err != nil {
		return err
	}
	allCodes := make([]string, 0, len(countryMap))
	for code := range countryMap {
		allCodes = append(allCodes, code)
	}
	writer, err := newWriter(allCodes)
	if err != nil {
		return err
	}
	err = write(writer, countryMap, "geoip.db", nil)
	if err != nil {
		return err
	}
	codes := []string{"cn", "private"}
	writer, err = newWriter(codes)
	if err != nil {
		return err
	}
	err = write(writer, countryMap, "geoip-only-cn-private.db", codes)
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	setActionOutput("tag", *sourceRelease.Name)
	return nil
}

func setActionOutput(name string, content string) {
	os.Stdout.WriteString("::set-output name=" + name + "::" + content + "\n")
}

func main() {
	var err error
	if len(os.Args) >= 3 {
		err = local(os.Args[1], os.Args[2], os.Args[3:])
	} else {
		err = release("v2fly/geoip", "maskedeken/sing-geoip")
	}
	if err != nil {
		logrus.Fatal(err)
	}
}
