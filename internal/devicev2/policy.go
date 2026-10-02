package devicev2

import "fmt"

type ACL struct {
	Direction string
	Topic     string
}

// DeriveACL is the sole mapping from the v2 interface to MQTT permissions.
func DeriveACL(deviceID string, manifest Manifest) ([]ACL, error) {
	if !identifier.MatchString(deviceID) {
		return nil, fmt.Errorf("device_id %q is invalid", deviceID)
	}
	acls := make([]ACL, 0, len(manifest.MQTT.Publish)+len(manifest.MQTT.Subscribe))
	for _, output := range manifest.MQTT.Publish {
		acls = append(acls, ACL{Direction: "write", Topic: "devices/" + deviceID + "/" + output.Channel})
	}
	for _, input := range manifest.MQTT.Subscribe {
		acls = append(acls, ACL{Direction: "read", Topic: "devices/" + deviceID + "/" + input.Channel})
	}
	return acls, nil
}

func PublishTopics(deviceID string, manifest Manifest) ([]string, error) {
	acls, err := DeriveACL(deviceID, manifest)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, acl := range acls {
		if acl.Direction == "write" {
			result = append(result, acl.Topic)
		}
	}
	return result, nil
}
