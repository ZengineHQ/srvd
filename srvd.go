package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/spf13/cobra"
)

var Image string
var Tag string
var Cluster string
var Services []string

//var Profile string
var Prefix string
var Region string
var ECRBase string

var ECS = &ecs.Client{}

var wg = sync.WaitGroup{}

func main() {
	var cmdDeploy = &cobra.Command{
		Use:   "deploy [ecr base url] [cluster name]",
		Short: "Deploy images image to ECS",
		Long:  "Deploy images images to ECS",
		Args:  cobra.MinimumNArgs(2),
		Run:   DeployCommand,
	}

	var baseCommand = &cobra.Command{Use: "app"}
	cmdDeploy.Flags().StringVarP(&Image, "image", "i", "", "override the image to use for all services")
	cmdDeploy.Flags().StringVarP(&Tag, "tag", "t", "latest", "The tag to deploy")
	cmdDeploy.Flags().StringArrayVarP(&Services, "service", "s", []string{}, "Services to deploy")
	//cmdDeploy.Flags().StringVarP(&Profile, "profile", "", "default", "AWS profile to use")
	cmdDeploy.Flags().StringVarP(&Prefix, "prefix", "p", "", "Prefix to use for services")
	cmdDeploy.Flags().StringVarP(&Region, "region", "r", "us-east-1", "Region to use")
	baseCommand.AddCommand(cmdDeploy)
	baseCommand.Execute()
}

func DeployCommand(cmd *cobra.Command, args []string) {
	ECRBase = args[0]
	Cluster = args[1]

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(Region),
	)
	ECS = ecs.NewFromConfig(cfg)

	if err != nil {
		log.Fatal("Error: ", err)
		return
	}

	killSig := make(chan bool)
	q := make(chan string)
	done := make(chan bool)

	numberOfWorkers := 3
	for i := 0; i < numberOfWorkers; i++ {
		wg.Add(1)
		go worker(q, i, done, killSig)
	}

	for _, service := range Services {
		fmt.Printf("Service: %s\n", service)
		go func(job string) {
			q <- job
		}(service)
	}

	numberOfJobs := len(Services)

	for c := 0; c < numberOfJobs; c++ {
		<-done
	}

	fmt.Println("Queue Done")

	close(killSig)

}

func worker(queue chan string, id int, done, ks chan bool) {
	defer wg.Done()
	for true {
		select {
		case job := <-queue:
			fmt.Printf("Worker(%d) Deploying: %s\n", id, job)
			deploy(job)
			fmt.Printf("Worker(%d) job done!\n", id)
			done <- true
		case <-ks:
			return
		case <-time.After(300 * time.Second):
			log.Printf("Worker %d timed out\n", id)
			return
		}
	}
}

func deploy(job string) {
	originalJob := job
	imageName := job

	if Image != "" {
		imageName = Image
	}

	if Prefix != "" {
		imageName = Prefix + imageName
		job = Prefix + job
	}

	imageUrl := fmt.Sprintf("%s/%s:%s", ECRBase, imageName, Tag)

	// Get service info

	serviceDesc, err := ECS.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
		Cluster: aws.String(Cluster),
		Services: []string{
			job,
		},
	})

	if err != nil {
		log.Fatal("Error: ", err)
		return
	}

	// Get task info
	log.Printf("Service %s cnt:%d", job, len(serviceDesc.Services))
	currentTaskArn := *serviceDesc.Services[0].TaskDefinition
	targetService := serviceDesc.Services[0]

	currentTaskDef, err := ECS.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(currentTaskArn),
	})

	if err != nil {
		log.Fatal("Error: ", err)
		return
	}

	for i, container := range currentTaskDef.TaskDefinition.ContainerDefinitions {
		if strings.Contains(*container.Image, job) {
			container.Image = &imageUrl
			currentTaskDef.TaskDefinition.ContainerDefinitions[i] = container
		}
	}

	// Create new taskdef and update service

	newDef := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions:    currentTaskDef.TaskDefinition.ContainerDefinitions,
		Family:                  currentTaskDef.TaskDefinition.Family,
		Volumes:                 currentTaskDef.TaskDefinition.Volumes,
		NetworkMode:             currentTaskDef.TaskDefinition.NetworkMode,
		TaskRoleArn:             currentTaskDef.TaskDefinition.TaskRoleArn,
		Cpu:                     currentTaskDef.TaskDefinition.Cpu,
		Memory:                  currentTaskDef.TaskDefinition.Memory,
		RequiresCompatibilities: currentTaskDef.TaskDefinition.RequiresCompatibilities,
		ExecutionRoleArn:        currentTaskDef.TaskDefinition.ExecutionRoleArn,
		PlacementConstraints:    currentTaskDef.TaskDefinition.PlacementConstraints,
	}

	registeredTask, err := ECS.RegisterTaskDefinition(context.TODO(), newDef)

	if err != nil {
		log.Fatal("Error: ", err)
	}

	_, err = ECS.UpdateService(context.TODO(), &ecs.UpdateServiceInput{
		Cluster:        aws.String(Cluster),
		Service:        aws.String(job),
		DesiredCount:   &targetService.DesiredCount,
		TaskDefinition: registeredTask.TaskDefinition.TaskDefinitionArn,
	})

	if err != nil {
		log.Fatal("Error: ", err)
		return
	}

	time.Sleep(2 * time.Second)

	statuses, err := getTaskStatus(targetService, *registeredTask.TaskDefinition)

	if err != nil {
		log.Fatal("Error: ", err)
		return
	}

	// TODO: Add a timeout
	for true {

		runningCount := 0
		statusText := ""

		for _, status := range statuses {
			if status == "RUNNING" {
				runningCount++
			}
			statusText += fmt.Sprintf("%s ", status)
		}

		log.Printf("%s => %s\n", *registeredTask.TaskDefinition.TaskDefinitionArn, statusText)

		if runningCount == len(statuses) && runningCount > 0 {
			fmt.Printf("%d tasks are now running for %s\n", runningCount, *targetService.ServiceName)
			return
		}

		statuses, err = getTaskStatus(targetService, *registeredTask.TaskDefinition)
		if err != nil {
			log.Fatal("Error: ", err)
			return
		}
		time.Sleep(5 * time.Second)
	}

	log.Printf("[%s] deployed => %s", originalJob, *registeredTask.TaskDefinition.TaskRoleArn)
	return
}

func getTaskStatus(service types.Service, taskDefinition types.TaskDefinition) (status []string, err error) {

	taskList, err := ECS.ListTasks(context.TODO(), &ecs.ListTasksInput{
		Cluster:     aws.String(Cluster),
		ServiceName: service.ServiceName,
	})

	if err != nil {
		return
	}

	tasks, err := ECS.DescribeTasks(context.TODO(), &ecs.DescribeTasksInput{
		Cluster: aws.String(Cluster),
		Tasks:   taskList.TaskArns,
	})

	if err != nil {
		return
	}

	for _, task := range tasks.Tasks {
		//log.Println(*task.TaskDefinitionArn)
		//log.Println(*taskDefinition.TaskDefinitionArn)
		if *task.TaskDefinitionArn == *taskDefinition.TaskDefinitionArn {
			status = append(status, *task.LastStatus)
		}
	}

	return
}
