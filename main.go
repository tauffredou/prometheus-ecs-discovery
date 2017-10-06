// Copyright 2017 Teralytics.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/go-yaml/yaml"
)

var outFile = flag.String("config.write-to", "ecs_file_sd.yml", "path of file to write ECS service discovery information to")
var interval = flag.Duration("config.scrape-interval", 60*time.Second, "interval at which to scrape the AWS API for ECS service discovery information")
var times = flag.Int("config.scrape-times", 0, "how many times to scrape before exiting (0 = infinite)")

func logError(err error) {
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case ecs.ErrCodeServerException:
				log.Println(ecs.ErrCodeServerException, aerr.Error())
			case ecs.ErrCodeClientException:
				log.Println(ecs.ErrCodeClientException, aerr.Error())
			case ecs.ErrCodeInvalidParameterException:
				log.Println(ecs.ErrCodeInvalidParameterException, aerr.Error())
			case ecs.ErrCodeClusterNotFoundException:
				log.Println(ecs.ErrCodeClusterNotFoundException, aerr.Error())
			default:
				log.Println(aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			log.Println(err.Error())
		}
	}
}

func GetClusters(svc *ecs.ECS) (*ecs.ListClustersOutput, error) {
	input := &ecs.ListClustersInput{}
	output := &ecs.ListClustersOutput{}
	for {
		myoutput, err := svc.ListClusters(input)
		if err != nil {
			return nil, err
		}
		output.ClusterArns = append(output.ClusterArns, myoutput.ClusterArns...)
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	return output, nil
}

type AugmentedTask struct {
	*ecs.Task
	TaskDefinition *ecs.TaskDefinition
	EC2Instance    *ec2.Instance
}

type PrometheusContainer struct {
	ContainerName string
	ContainerArn  string
	DockerImage   string
	Port          int
}

// ExportedContainers returns a list of []*PrometheusContainer
// enumerating the ports that the container exports for
// Prometheus, one per container, so long as the Docker
// labels in its corresponding container definition have
// a PROMETHEUS_EXPORTER_PORT_INDEX corresponding to an
// existing port mapping index for that container.
//
// Thus, a task with a container definition that has
//     ...
//             "Name": "mosquitto",
//             "DockerLabels": {
//                  "PROMETHEUS_EXPORTER_PORTINDEX": "1"
//              },
//     ...
//              "PortMappings": [
//                {
//                  "ContainerPort": 1883,
//                  "HostPort": 0,
//                  "Protocol": "tcp"
//                },
//                {
//                  "ContainerPort": 9001,
//                  "HostPort": 0,
//                  "Protocol": "tcp"
//                }
//              ],
//     ...
// would see its second port (whatever the host port was
// for the running container that got mapped to port 9001)
// exposed as a mapped Prometheus port.
func (t *AugmentedTask) ExportedContainers() []*PrometheusContainer {
	instances := []*PrometheusContainer{}
	for _, c := range t.Containers {
		var d *ecs.ContainerDefinition
		for _, d = range t.TaskDefinition.ContainerDefinitions {
			if *c.Name == *d.Name {
				break
			}
		}
		if *c.Name != *d.Name {
			continue
		}
		var v *string
		var ok bool
		if v, ok = d.DockerLabels["PROMETHEUS_EXPORTER_PORT_INDEX"]; !ok {
			continue
		}
		var err error
		var portindex int
		if portindex, err = strconv.Atoi(*v); err != nil || portindex < 0 || portindex >= len(d.PortMappings) {
			continue
		}
		instances = append(instances, &PrometheusContainer{*c.Name, *c.ContainerArn, *d.Image, int(*c.NetworkBindings[portindex].HostPort)})
	}
	return instances
}

type PrometheusTaskInfo struct {
	Targets []string      `yaml:"targets"`
	Labels  yaml.MapSlice `yaml:"labels"`
}

func (t *AugmentedTask) ExporterInformation() []*PrometheusTaskInfo {
	ret := []*PrometheusTaskInfo{}
	instances := t.ExportedContainers()
	if len(instances) == 0 {
		return ret
	}
	var ip string
	if t.EC2Instance == nil {
		return ret
	}
	if len(t.EC2Instance.NetworkInterfaces) == 0 {
		return ret
	}
	for _, iface := range t.EC2Instance.NetworkInterfaces {
		if iface.PrivateIpAddress != nil && *iface.PrivateIpAddress != "" {
			ip = *iface.PrivateIpAddress
			break
		}
	}
	if ip == "" {
		return ret
	}
	for _, i := range instances {
		labels := yaml.MapSlice{}
		labels = append(labels,
			yaml.MapItem{"task_arn", *t.TaskArn},
			yaml.MapItem{"task_name", *t.TaskDefinition.Family},
			yaml.MapItem{"task_revision", fmt.Sprintf("%d", *t.TaskDefinition.Revision)},
			yaml.MapItem{"task_group", *t.Group},
			yaml.MapItem{"cluster_arn", *t.ClusterArn},
			yaml.MapItem{"container_name", i.ContainerName},
			yaml.MapItem{"container_arn", i.ContainerArn},
			yaml.MapItem{"docker_image", i.DockerImage},
		)
		ret = append(ret, &PrometheusTaskInfo{
			Targets: []string{fmt.Sprintf("%s:%d", ip, i.Port)},
			Labels:  labels,
		})
	}
	return ret
}

func AddTaskDefinitionsOfTasks(svc *ecs.ECS, taskList []*AugmentedTask) ([]*AugmentedTask, error) {
	task2def := make(map[string]*ecs.TaskDefinition)
	for _, task := range taskList {
		task2def[*task.TaskDefinitionArn] = nil
	}

	jobs := make(chan *ecs.DescribeTaskDefinitionInput, len(task2def))
	results := make(chan struct {
		out *ecs.DescribeTaskDefinitionOutput
		err error
	}, len(task2def))

	for w := 1; w <= 4; w++ {
		go func() {
			for in := range jobs {
				out, err := svc.DescribeTaskDefinition(in)
				results <- struct {
					out *ecs.DescribeTaskDefinitionOutput
					err error
				}{out, err}
			}
		}()
	}

	for tn := range task2def {
		m := string(append([]byte{}, tn...))
		jobs <- &ecs.DescribeTaskDefinitionInput{TaskDefinition: &m}
	}
	close(jobs)

	var err error
	for range task2def {
		result := <-results
		if result.err != nil {
			err = result.err
			log.Printf("Error describing task definition: %s", err)
		} else {
			log.Printf("Described task definition %s", *result.out.TaskDefinition.TaskDefinitionArn)
			task2def[*result.out.TaskDefinition.TaskDefinitionArn] = result.out.TaskDefinition
		}
	}
	if err != nil {
		return nil, err
	}

	for _, task := range taskList {
		task.TaskDefinition = task2def[*task.TaskDefinitionArn]
	}
	return taskList, nil
}

func StringToStarString(s []string) []*string {
	c := make([]*string, 0, len(s))
	for n, _ := range s {
		c = append(c, &s[n])
	}
	return c
}

func DescribeInstancesUnpaginated(svcec2 *ec2.EC2, instanceIds []string) ([]*ec2.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		InstanceIds: StringToStarString(instanceIds),
	}
	finalOutput := &ec2.DescribeInstancesOutput{}
	for {
		output, err := svcec2.DescribeInstances(input)
		if err != nil {
			return nil, err
		}
		log.Printf("Described %d EC2 reservations", len(output.Reservations))
		finalOutput.Reservations = append(finalOutput.Reservations, output.Reservations...)
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}
	result := []*ec2.Instance{}
	for _, rsv := range finalOutput.Reservations {
		for _, i := range rsv.Instances {
			result = append(result, i)
		}
	}
	return result, nil
}

func AddContainerInstancesToTasks(svc *ecs.ECS, svcec2 *ec2.EC2, taskList []*AugmentedTask) ([]*AugmentedTask, error) {
	clusterArnToContainerInstancesArns := make(map[string]map[string]*ecs.ContainerInstance)
	for _, task := range taskList {
		if _, ok := clusterArnToContainerInstancesArns[*task.ClusterArn]; !ok {
			clusterArnToContainerInstancesArns[*task.ClusterArn] = make(map[string]*ecs.ContainerInstance)
		}
		clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn] = nil
	}

	instanceIDToEC2Instance := make(map[string]*ec2.Instance)
	for clusterArn, containerInstancesArns := range clusterArnToContainerInstancesArns {
		keys := make([]string, 0, len(containerInstancesArns))
		for k := range containerInstancesArns {
			keys = append(keys, k)
		}
		input := &ecs.DescribeContainerInstancesInput{
			Cluster:            &clusterArn,
			ContainerInstances: StringToStarString(keys),
		}
		output, err := svc.DescribeContainerInstances(input)
		if err != nil {
			return nil, err
		}
		log.Printf("Described %d container instances in cluster %s", len(output.ContainerInstances), clusterArn)
		if len(output.Failures) > 0 {
			log.Printf("Described %d failures in cluster %s", len(output.Failures), clusterArn)
		}
		for _, ci := range output.ContainerInstances {
			clusterArnToContainerInstancesArns[clusterArn][*ci.ContainerInstanceArn] = ci
			instanceIDToEC2Instance[*ci.Ec2InstanceId] = nil
		}
	}

	keys := make([]string, 0, len(instanceIDToEC2Instance))
	for id, _ := range instanceIDToEC2Instance {
		keys = append(keys, id)
	}

	instances, err := DescribeInstancesUnpaginated(svcec2, keys)
	if err != nil {
		return nil, err
	}

	for _, i := range instances {
		instanceIDToEC2Instance[*i.InstanceId] = i
	}

	for _, task := range taskList {
		containerInstance, ok := clusterArnToContainerInstancesArns[*task.ClusterArn][*task.ContainerInstanceArn]
		if !ok {
			log.Printf("Cannot find container instance %s in cluster %s", *task.ContainerInstanceArn, *task.ClusterArn)
			continue
		}
		instance, ok := instanceIDToEC2Instance[*containerInstance.Ec2InstanceId]
		if !ok {
			log.Printf("Cannot find EC2 instance", *containerInstance.Ec2InstanceId)
			continue
		}
		task.EC2Instance = instance
	}

	return taskList, nil
}

func GetTasksOfClusters(svc *ecs.ECS, svcec2 *ec2.EC2, clusterArns []*string) ([]*ecs.Task, error) {
	jobs := make(chan *string, len(clusterArns))
	results := make(chan struct {
		out *ecs.DescribeTasksOutput
		err error
	}, len(clusterArns))

	for w := 1; w <= 4; w++ {
		go func() {
			for clusterArn := range jobs {
				input := &ecs.ListTasksInput{
					Cluster: clusterArn,
				}
				finalOutput := &ecs.DescribeTasksOutput{}
				var err error
				for {
					output, err1 := svc.ListTasks(input)
					if err != nil {
						err = err1
						log.Printf("Error listing tasks of cluster %s: %s", *clusterArn, err)
						break
					}
					if len(output.TaskArns) == 0 {
						break
					}
					log.Printf("Inspected cluster %s, found %d tasks", *clusterArn, len(output.TaskArns))
					descOutput, err2 := svc.DescribeTasks(&ecs.DescribeTasksInput{
						Cluster: clusterArn,
						Tasks:   output.TaskArns,
					})
					if err2 != nil {
						err = err2
						log.Printf("Error describing tasks of cluster %s: %s", *clusterArn, err)
						break
					}
					log.Printf("Described %d tasks in cluster %s", len(descOutput.Tasks), *clusterArn)
					if len(descOutput.Failures) > 0 {
						log.Printf("Described %d failures in cluster %s", len(descOutput.Failures), *clusterArn)
					}
					finalOutput.Tasks = append(finalOutput.Tasks, descOutput.Tasks...)
					finalOutput.Failures = append(finalOutput.Failures, descOutput.Failures...)
					if output.NextToken == nil {
						break
					}
					input.NextToken = output.NextToken
				}
				results <- struct {
					out *ecs.DescribeTasksOutput
					err error
				}{finalOutput, err}
			}
		}()
	}

	for _, clusterArn := range clusterArns {
		jobs <- clusterArn
	}
	close(jobs)

	tasks := []*ecs.Task{}
	for range clusterArns {
		result := <-results
		if result.err != nil {
			return nil, result.err
		} else {
			for _, task := range result.out.Tasks {
				tasks = append(tasks, task)
			}
		}
	}

	return tasks, nil
}

func GetAugmentedTasks(svc *ecs.ECS, svcec2 *ec2.EC2, clusterArns []*string) ([]*AugmentedTask, error) {
	simpleTasks, err := GetTasksOfClusters(svc, svcec2, clusterArns)
	if err != nil {
		return nil, err
	}

	tasks := []*AugmentedTask{}
	for _, t := range simpleTasks {
		tasks = append(tasks, &AugmentedTask{t, nil, nil})
	}

	tasks, err = AddTaskDefinitionsOfTasks(svc, tasks)
	if err != nil {
		return nil, err
	}

	tasks, err = AddContainerInstancesToTasks(svc, svcec2, tasks)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

func main() {
	flag.Parse()
	sess := session.New()
	svc := ecs.New(sess)
	svcec2 := ec2.New(sess)
	work := func() {
		clusters, err := GetClusters(svc)
		if err != nil {
			logError(err)
			return
		}
		tasks, err := GetAugmentedTasks(svc, svcec2, clusters.ClusterArns)
		if err != nil {
			logError(err)
			return
		}
		infos := []*PrometheusTaskInfo{}
		for _, t := range tasks {
			info := t.ExporterInformation()
			infos = append(infos, info...)
		}
		m, err := yaml.Marshal(infos)
		if err != nil {
			logError(err)
			return
		}
		log.Printf("Writing %d discovered exporters to %s", len(infos), *outFile)
		err = ioutil.WriteFile(*outFile, m, 0644)
		if err != nil {
			logError(err)
			return
		}
	}
	s := time.NewTimer(1 * time.Millisecond)
	t := time.NewTicker(*interval)
	n := *times
	for {
		select {
		case <-s.C:
		case <-t.C:
		}
		work()
		n = n - 1
		if *times > 0 && n == 0 {
			break
		}
	}
}
