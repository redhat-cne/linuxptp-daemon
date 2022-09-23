package daemon

import (
	"errors"
	"fmt"
	"strings"
)

type syncE4lConfSection struct {
	sectionName string
	options     map[string]string
}

type syncE4lConf struct {
	sections    []syncE4lConfSection
	mapping     []string
	profileName string
}

func (conf *syncE4lConf) populateSyncE4lConf(config *string) error {
	lines := strings.Split(*config, "\n")
	var currentSectionName string
	var currentSection syncE4lConfSection
	conf.sections = make([]syncE4lConfSection, 0)
	globalIsDefined := false

	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		} else if strings.HasPrefix(line, "[") {
			if currentSectionName != "" {
				conf.sections = append(conf.sections, currentSection)
			}
			currentSectionName = line
			currentLine := strings.Split(line, "]")

			if len(currentLine) < 2 {
				return errors.New("Section missing closing ']'")
			}

			currentSectionName = fmt.Sprintf("%s]", currentLine[0])
			if currentSectionName == "[global]" {
				globalIsDefined = true
			}
			currentSection = syncE4lConfSection{options: map[string]string{}, sectionName: currentSectionName}
		} else if currentSectionName != "" {
			split := strings.IndexByte(line, ' ')
			if split > 0 {
				currentSection.options[line[:split]] = line[split:]
			}
		} else {
			return errors.New("config option not in section")
		}
	}
	if currentSectionName != "" {
		conf.sections = append(conf.sections, currentSection)
	}
	if !globalIsDefined {
		conf.sections = append(conf.sections, syncE4lConfSection{options: map[string]string{}, sectionName: "[global]"})
	}
	return nil
}

func (conf *syncE4lConf) renderSyncE4lConf() string {
	configOut := fmt.Sprintf("#profile: %s\n", conf.profileName)
	conf.mapping = nil

	for _, section := range conf.sections {
		configOut = fmt.Sprintf("%s\n%s", configOut, section.sectionName)
		for k, v := range section.options {
			configOut = fmt.Sprintf("%s\n%s %s", configOut, k, v)
		}
	}
	return configOut
}
