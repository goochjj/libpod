package generate

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/containers/libpod/libpod/image"
	"github.com/containers/libpod/pkg/specgen"
	"github.com/cri-o/ocicni/pkg/ocicni"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	protoTCP  = "tcp"
	protoUDP  = "udp"
	protoSCTP = "sctp"
)

// Parse port maps to OCICNI port mappings.
// Returns a set of OCICNI port mappings, and maps of utilized container and
// host ports.
func parsePortMapping(portMappings []specgen.PortMapping) ([]ocicni.PortMapping, map[string]map[string]map[uint16]uint16, map[string]map[string]map[uint16]uint16, error) {
	// First, we need to validate the ports passed in the specgen, and then
	// convert them into CNI port mappings.
	finalMappings := []ocicni.PortMapping{}

	// To validate, we need two maps: one for host ports, one for container
	// ports.
	// Each is a map of protocol to map of IP address to map of port to
	// port (for hostPortValidate, it's host port to container port;
	// for containerPortValidate, container port to host port.
	// These will ensure no collisions.
	hostPortValidate := make(map[string]map[string]map[uint16]uint16)
	containerPortValidate := make(map[string]map[string]map[uint16]uint16)

	// Initialize the first level of maps (we can't really guess keys for
	// the rest).
	for _, proto := range []string{protoTCP, protoUDP, protoSCTP} {
		hostPortValidate[proto] = make(map[string]map[uint16]uint16)
		containerPortValidate[proto] = make(map[string]map[uint16]uint16)
	}

	// Iterate through all port mappings, generating OCICNI PortMapping
	// structs and validating there is no overlap.
	for _, port := range portMappings {
		// First, check proto
		protocols, err := checkProtocol(port.Protocol, true)
		if err != nil {
			return nil, nil, nil, err
		}

		// Validate host IP
		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		if ip := net.ParseIP(hostIP); ip == nil {
			return nil, nil, nil, errors.Errorf("invalid IP address %s in port mapping", port.HostIP)
		}

		// Validate port numbers and range.
		len := port.Range
		if len == 0 {
			len = 1
		}
		containerPort := port.ContainerPort
		if containerPort == 0 {
			return nil, nil, nil, errors.Errorf("container port number must be non-0")
		}
		hostPort := port.HostPort
		if hostPort == 0 {
			hostPort = containerPort
		}
		if uint32(len-1)+uint32(containerPort) > 65535 {
			return nil, nil, nil, errors.Errorf("container port range exceeds maximum allowable port number")
		}
		if uint32(len-1)+uint32(hostPort) > 65536 {
			return nil, nil, nil, errors.Errorf("host port range exceeds maximum allowable port number")
		}

		// Iterate through ports, populating maps to check for conflicts
		// and generating CNI port mappings.
		for _, p := range protocols {
			hostIPMap := hostPortValidate[p]
			ctrIPMap := containerPortValidate[p]

			hostPortMap, ok := hostIPMap[hostIP]
			if !ok {
				hostPortMap = make(map[uint16]uint16)
				hostIPMap[hostIP] = hostPortMap
			}
			ctrPortMap, ok := ctrIPMap[hostIP]
			if !ok {
				ctrPortMap = make(map[uint16]uint16)
				ctrIPMap[hostIP] = ctrPortMap
			}

			// Iterate through all port numbers in the requested
			// range.
			var index uint16
			for index = 0; index < len; index++ {
				cPort := containerPort + index
				hPort := hostPort + index

				if cPort == 0 || hPort == 0 {
					return nil, nil, nil, errors.Errorf("host and container ports cannot be 0")
				}

				testCPort := ctrPortMap[cPort]
				if testCPort != 0 && testCPort != hPort {
					// This is an attempt to redefine a port
					return nil, nil, nil, errors.Errorf("conflicting port mappings for container port %d (protocol %s)", cPort, p)
				}
				ctrPortMap[cPort] = hPort

				testHPort := hostPortMap[hPort]
				if testHPort != 0 && testHPort != cPort {
					return nil, nil, nil, errors.Errorf("conflicting port mappings for host port %d (protocol %s)", hPort, p)
				}
				hostPortMap[hPort] = cPort

				// If we have an exact duplicate, just continue
				if testCPort == hPort && testHPort == cPort {
					continue
				}

				// We appear to be clear. Make an OCICNI port
				// struct.
				// Don't use hostIP - we want to preserve the
				// empty string hostIP by default for compat.
				cniPort := ocicni.PortMapping{
					HostPort:      int32(hPort),
					ContainerPort: int32(cPort),
					Protocol:      p,
					HostIP:        port.HostIP,
				}
				finalMappings = append(finalMappings, cniPort)
			}
		}
	}

	return finalMappings, containerPortValidate, hostPortValidate, nil
}

// Make final port mappings for the container
func createPortMappings(ctx context.Context, s *specgen.SpecGenerator, img *image.Image) ([]ocicni.PortMapping, error) {
	finalMappings, containerPortValidate, hostPortValidate, err := parsePortMapping(s.PortMappings)
	if err != nil {
		return nil, err
	}

	// If not publishing exposed ports, or if we are publishing and there is
	// nothing to publish - then just return the port mappings we've made so
	// far.
	if !s.PublishExposedPorts || (len(s.Expose) == 0 && img == nil) {
		return finalMappings, nil
	}

	logrus.Debugf("Adding exposed ports")

	// We need to merge s.Expose into image exposed ports
	expose := make(map[uint16]string)
	for k, v := range s.Expose {
		expose[k] = v
	}
	if img != nil {
		inspect, err := img.InspectNoSize(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "error inspecting image to get exposed ports")
		}
		for imgExpose := range inspect.Config.ExposedPorts {
			// Expose format is portNumber[/protocol]
			splitExpose := strings.SplitN(imgExpose, "/", 2)
			num, err := strconv.Atoi(splitExpose[0])
			if err != nil {
				return nil, errors.Wrapf(err, "unable to convert image EXPOSE statement %q to port number", imgExpose)
			}
			if num > 65535 || num < 1 {
				return nil, errors.Errorf("%d from image EXPOSE statement %q is not a valid port number", num, imgExpose)
			}
			// No need to validate protocol, we'll do it below.
			if len(splitExpose) == 1 {
				expose[uint16(num)] = "tcp"
			} else {
				expose[uint16(num)] = splitExpose[1]
			}
		}
	}

	// There's been a request to expose some ports. Let's do that.
	// Start by figuring out what needs to be exposed.
	// This is a map of container port number to protocols to expose.
	toExpose := make(map[uint16][]string)
	for port, proto := range expose {
		// Validate protocol first
		protocols, err := checkProtocol(proto, false)
		if err != nil {
			return nil, errors.Wrapf(err, "error validating protocols for exposed port %d", port)
		}

		if port == 0 {
			return nil, errors.Errorf("cannot expose 0 as it is not a valid port number")
		}

		// Check to see if the port is already present in existing
		// mappings.
		for _, p := range protocols {
			ctrPortMap, ok := containerPortValidate[p]["0.0.0.0"]
			if !ok {
				ctrPortMap = make(map[uint16]uint16)
				containerPortValidate[p]["0.0.0.0"] = ctrPortMap
			}

			if portNum := ctrPortMap[port]; portNum == 0 {
				// We want to expose this port for this protocol
				exposeProto, ok := toExpose[port]
				if !ok {
					exposeProto = []string{}
				}
				exposeProto = append(exposeProto, p)
				toExpose[port] = exposeProto
			}
		}
	}

	// We now have a final list of ports that we want exposed.
	// Let's find empty, unallocated host ports for them.
	for port, protocols := range toExpose {
		for _, p := range protocols {
			// Find an open port on the host.
			// I see a faint possibility that this will infinite
			// loop trying to find a valid open port, so I've
			// included a max-tries counter.
			hostPort := 0
			tries := 15
			for hostPort == 0 && tries > 0 {
				// We can't select a specific protocol, which is
				// unfortunate for the UDP case.
				candidate, err := getRandomPort()
				if err != nil {
					return nil, err
				}

				// Check if the host port is already bound
				hostPortMap, ok := hostPortValidate[p]["0.0.0.0"]
				if !ok {
					hostPortMap = make(map[uint16]uint16)
					hostPortValidate[p]["0.0.0.0"] = hostPortMap
				}

				if checkPort := hostPortMap[uint16(candidate)]; checkPort != 0 {
					// Host port is already allocated, try again
					tries--
					continue
				}

				hostPortMap[uint16(candidate)] = port
				hostPort = candidate
				logrus.Debugf("Mapping exposed port %d/%s to host port %d", port, p, hostPort)

				// Make a CNI port mapping
				cniPort := ocicni.PortMapping{
					HostPort:      int32(candidate),
					ContainerPort: int32(port),
					Protocol:      p,
					HostIP:        "",
				}
				finalMappings = append(finalMappings, cniPort)
			}
			if tries == 0 && hostPort == 0 {
				// We failed to find an open port.
				return nil, errors.Errorf("failed to find an open port to expose container port %d on the host", port)
			}
		}
	}

	return finalMappings, nil
}

// Check a string to ensure it is a comma-separated set of valid protocols
func checkProtocol(protocol string, allowSCTP bool) ([]string, error) {
	protocols := make(map[string]struct{})
	splitProto := strings.Split(protocol, ",")
	// Don't error on duplicates - just deduplicate
	for _, p := range splitProto {
		switch p {
		case protoTCP, "":
			protocols[protoTCP] = struct{}{}
		case protoUDP:
			protocols[protoUDP] = struct{}{}
		case protoSCTP:
			if !allowSCTP {
				return nil, errors.Errorf("protocol SCTP is not allowed for exposed ports")
			}
			protocols[protoSCTP] = struct{}{}
		default:
			return nil, errors.Errorf("unrecognized protocol %q in port mapping", p)
		}
	}

	finalProto := []string{}
	for p := range protocols {
		finalProto = append(finalProto, p)
	}

	// This shouldn't be possible, but check anyways
	if len(finalProto) == 0 {
		return nil, errors.Errorf("no valid protocols specified for port mapping")
	}

	return finalProto, nil
}

// Find a random, open port on the host
func getRandomPort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, errors.Wrapf(err, "unable to get free TCP port")
	}
	defer l.Close()
	_, randomPort, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return 0, errors.Wrapf(err, "unable to determine free port")
	}
	rp, err := strconv.Atoi(randomPort)
	if err != nil {
		return 0, errors.Wrapf(err, "unable to convert random port to int")
	}
	return rp, nil
}