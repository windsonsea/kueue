/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/controller/constants"
	workloadjob "sigs.k8s.io/kueue/pkg/controller/jobs/job"
	"sigs.k8s.io/kueue/pkg/util/slices"
	"sigs.k8s.io/kueue/pkg/util/testing"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	"sigs.k8s.io/kueue/pkg/workload"
	"sigs.k8s.io/kueue/test/util"
)

var _ = ginkgo.Describe("Kueue", func() {
	var ns *corev1.Namespace
	var sampleJob *batchv1.Job
	var jobKey types.NamespacedName

	ginkgo.BeforeEach(func() {
		ns = util.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "e2e-")
		sampleJob = testingjob.MakeJob("test-job", ns.Name).
			Queue("main").
			RequestAndLimit(corev1.ResourceCPU, "1").
			RequestAndLimit(corev1.ResourceMemory, "20Mi").
			Obj()
		jobKey = client.ObjectKeyFromObject(sampleJob)
	})
	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		util.ExpectAllPodsInNamespaceDeleted(ctx, k8sClient, ns)
	})

	ginkgo.When("Creating a Job without a matching LocalQueue", func() {
		ginkgo.It("Should stay in suspended", func() {
			util.MustCreate(ctx, k8sClient, sampleJob)

			createdJob := &batchv1.Job{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
				g.Expect(*createdJob.Spec.Suspend).Should(gomega.BeTrue())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
			createdWorkload := &kueue.Workload{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
				g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeFalse())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
			gomega.Expect(k8sClient.Delete(ctx, sampleJob)).Should(gomega.Succeed())
		})
	})

	ginkgo.When("Creating a Job With Queueing", func() {
		var (
			onDemandRF   *kueue.ResourceFlavor
			spotRF       *kueue.ResourceFlavor
			localQueue   *kueue.LocalQueue
			clusterQueue *kueue.ClusterQueue
		)
		ginkgo.BeforeEach(func() {
			onDemandRF = testing.MakeResourceFlavor("on-demand").
				NodeLabel("instance-type", "on-demand").Obj()
			util.MustCreate(ctx, k8sClient, onDemandRF)
			spotRF = testing.MakeResourceFlavor("spot").
				NodeLabel("instance-type", "spot").Obj()
			util.MustCreate(ctx, k8sClient, spotRF)
			clusterQueue = testing.MakeClusterQueue("cluster-queue").
				ResourceGroup(
					*testing.MakeFlavorQuotas("on-demand").
						Resource(corev1.ResourceCPU, "1").
						Resource(corev1.ResourceMemory, "1Gi").
						Obj(),
					*testing.MakeFlavorQuotas("spot").
						Resource(corev1.ResourceCPU, "1").
						Resource(corev1.ResourceMemory, "1Gi").
						Obj(),
				).
				Preemption(kueue.ClusterQueuePreemption{
					WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
				}).
				Obj()
			util.MustCreate(ctx, k8sClient, clusterQueue)
			localQueue = testing.MakeLocalQueue("main", ns.Name).ClusterQueue("cluster-queue").Obj()
			util.MustCreate(ctx, k8sClient, localQueue)
		})
		ginkgo.AfterEach(func() {
			gomega.Expect(util.DeleteAllCronJobsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			gomega.Expect(util.DeleteAllJobsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			// Force remove workloads to be sure that cluster queue can be removed.
			gomega.Expect(util.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			gomega.Expect(util.DeleteObject(ctx, k8sClient, localQueue)).Should(gomega.Succeed())
			util.ExpectObjectToBeDeleted(ctx, k8sClient, clusterQueue, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, onDemandRF, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, spotRF, true)
			util.ExpectAllPodsInNamespaceDeleted(ctx, k8sClient, ns)
		})

		ginkgo.It("Should allow to schedule Jobs via CronJob", func() {
			cronJob := &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cronjob",
					Namespace: ns.Name,
				},
				Spec: batchv1.CronJobSpec{
					Schedule:          "* * * * *",
					ConcurrencyPolicy: batchv1.ForbidConcurrent,
					JobTemplate: batchv1.JobTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								constants.QueueLabel: localQueue.Name,
							},
						},
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									Containers: []corev1.Container{
										{
											Name:    "c",
											Image:   util.GetAgnHostImage(),
											Command: util.BehaviorExitFast,
											Resources: corev1.ResourceRequirements{
												Requests: corev1.ResourceList{
													corev1.ResourceCPU: resource.MustParse("1"),
												},
												Limits: corev1.ResourceList{
													corev1.ResourceCPU: resource.MustParse("1"),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			util.MustCreate(ctx, k8sClient, cronJob)

			ginkgo.By("Patch the last start time to be in the past so that it starts immediately", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cronJob), cronJob)).To(gomega.Succeed())
					nextSchedule := cronJob.CreationTimestamp.Add(-2 * time.Minute)
					cronJob.Status.LastScheduleTime = ptr.To(metav1.Time{Time: nextSchedule})
					g.Expect(k8sClient.Status().Update(ctx, cronJob)).Should(gomega.Succeed())
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})

			createJobs := &batchv1.JobList{}
			ginkgo.By("Check that the Job is create and retrieve it", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.List(ctx, createJobs, client.InNamespace(ns.Name))).To(gomega.Succeed())
					g.Expect(createJobs.Items).To(gomega.HaveLen(1))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})

			createdJob := createJobs.Items[0]
			ginkgo.By("verify the job has the nodeSelector assigned", func() {
				jobKey := client.ObjectKeyFromObject(&createdJob)
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "on-demand",
				})
			})
			ginkgo.By("verify the workload was created and admitted for the Job", func() {
				createdWorkload := &kueue.Workload{}
				wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(createdJob.Name, createdJob.UID), Namespace: ns.Name}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeTrue())
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("Should unsuspend a job and set nodeSelectors", func() {
			// Use a binary that ends.
			sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).Image(util.GetAgnHostImage(), util.BehaviorExitFast).Obj()
			util.MustCreate(ctx, k8sClient, sampleJob)

			createdWorkload := &kueue.Workload{}

			// The job might have finished at this point. That shouldn't be a problem for the purpose of this test
			util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
				"instance-type": "on-demand",
			})
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
				g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeTrue())
				g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadFinished))
			}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.It("Should run with prebuilt workload", func() {
			var wl *kueue.Workload
			ginkgo.By("Create the pebuilt workload and the job adopting it", func() {
				sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
					Label(constants.PrebuiltWorkloadLabel, "prebuilt-wl").
					BackoffLimit(0).
					Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletionFailOnExit).
					TerminationGracePeriod(1).
					Obj()
				testingjob.SetContainerDefaults(&sampleJob.Spec.Template.Spec.Containers[0])

				wl = testing.MakeWorkload("prebuilt-wl", ns.Name).
					Finalizers(kueue.ResourceInUseFinalizerName).
					Queue(kueue.LocalQueueName(localQueue.Name)).
					PodSets(
						*testing.MakePodSet(kueue.DefaultPodSetName, 1).Containers(sampleJob.Spec.Template.Spec.Containers[0]).Obj(),
					).
					Obj()
				util.MustCreate(ctx, k8sClient, wl)
				util.MustCreate(ctx, k8sClient, sampleJob)
			})

			createdWorkload := &kueue.Workload{}
			wlLookupKey := client.ObjectKeyFromObject(wl)
			createdJob := &batchv1.Job{}
			jobLookupKey := client.ObjectKeyFromObject(sampleJob)

			ginkgo.By("Verify the prebuilt workload is adopted by the job", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobLookupKey, createdJob)).To(gomega.Succeed())
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).To(gomega.Succeed())
					g.Expect(wl.Spec.PodSets[0].Template.Spec.Containers).To(gomega.BeComparableTo(createdJob.Spec.Template.Spec.Containers), "Check the way the job and workload is created")
					util.MustHaveOwnerReference(g, createdWorkload.OwnerReferences, sampleJob, k8sClient.Scheme())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Verify the job is running", func() {
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "on-demand",
				})
			})

			ginkgo.By("Await for pods to be running", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					var job batchv1.Job
					g.Expect(k8sClient.Get(ctx, jobKey, &job)).To(gomega.Succeed())
					g.Expect(job.Status.Active).To(gomega.BeEquivalentTo(1))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Delete all pods", func() {
				gomega.Expect(util.DeleteAllPodsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			})

			ginkgo.By("Await for jobs completion", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), createdWorkload)).To(gomega.Succeed())
					g.Expect(createdWorkload.Finalizers).NotTo(gomega.ContainElement(kueue.ResourceInUseFinalizerName))
					g.Expect(createdWorkload.Status.Conditions).To(testing.HaveConditionStatusTrueAndReason(kueue.WorkloadFinished, kueue.WorkloadFinishedReasonFailed))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("Should readmit preempted job with priorityClass into a separate flavor", func() {
			util.MustCreate(ctx, k8sClient, sampleJob)

			highPriorityClass := testing.MakePriorityClass("high").PriorityValue(100).Obj()
			util.MustCreate(ctx, k8sClient, highPriorityClass)
			ginkgo.DeferCleanup(func() {
				gomega.Expect(k8sClient.Delete(ctx, highPriorityClass)).To(gomega.Succeed())
			})

			ginkgo.By("Job is admitted using the first flavor", func() {
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "on-demand",
				})
			})

			ginkgo.By("Job is preempted by higher priority job", func() {
				job := testingjob.MakeJob("high", ns.Name).
					Queue("main").
					PriorityClass("high").
					RequestAndLimit(corev1.ResourceCPU, "1").
					NodeSelector("instance-type", "on-demand"). // target the same flavor to cause preemption
					Obj()
				util.MustCreate(ctx, k8sClient, job)

				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, client.ObjectKeyFromObject(job), map[string]string{
					"instance-type": "on-demand",
				})
			})

			ginkgo.By("Job is re-admitted using the second flavor", func() {
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "spot",
				})
			})
		})

		ginkgo.It("Should readmit preempted job with workloadPriorityClass into a separate flavor", func() {
			util.MustCreate(ctx, k8sClient, sampleJob)

			highWorkloadPriorityClass := testing.MakeWorkloadPriorityClass("high-workload").PriorityValue(300).Obj()
			util.MustCreate(ctx, k8sClient, highWorkloadPriorityClass)
			ginkgo.DeferCleanup(func() {
				gomega.Expect(k8sClient.Delete(ctx, highWorkloadPriorityClass)).To(gomega.Succeed())
			})

			ginkgo.By("Job is admitted using the first flavor", func() {
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "on-demand",
				})
			})

			ginkgo.By("Job is preempted by higher priority job", func() {
				job := testingjob.MakeJob("high-with-wpc", ns.Name).
					Queue("main").
					WorkloadPriorityClass("high-workload").
					RequestAndLimit(corev1.ResourceCPU, "1").
					NodeSelector("instance-type", "on-demand"). // target the same flavor to cause preemption
					Obj()
				util.MustCreate(ctx, k8sClient, job)

				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, client.ObjectKeyFromObject(job), map[string]string{
					"instance-type": "on-demand",
				})
			})

			ginkgo.By("Job is re-admitted using the second flavor", func() {
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "spot",
				})
			})
		})
		ginkgo.It("Should partially admit the Job if configured and not fully fits", func() {
			// Use a binary that ends.
			job := testingjob.MakeJob("job", ns.Name).
				Queue("main").
				Image(util.GetAgnHostImage(), util.BehaviorExitFast).
				RequestAndLimit(corev1.ResourceCPU, "500m").
				Parallelism(3).
				Completions(4).
				SetAnnotation(workloadjob.JobMinParallelismAnnotation, "1").
				Obj()
			util.MustCreate(ctx, k8sClient, job)

			// The job might have finished at this point. That shouldn't be a problem for the purpose of this test
			ginkgo.By("Wait for the job to start and check the updated Parallelism and Completions", func() {
				jobKey := client.ObjectKeyFromObject(job)
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "on-demand",
				})

				updatedJob := &batchv1.Job{}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, updatedJob)).To(gomega.Succeed())
					g.Expect(ptr.Deref(updatedJob.Spec.Parallelism, 0)).To(gomega.Equal(int32(2)))
					g.Expect(ptr.Deref(updatedJob.Spec.Completions, 0)).To(gomega.Equal(int32(4)))
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Wait for the job to finish", func() {
				createdWorkload := &kueue.Workload{}
				wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(job.Name, job.UID), Namespace: ns.Name}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeTrue())
					g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadFinished))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("Should allow updating the workload's priority through the job", func() {
			lowPriority := "low-priority"
			lowPriorityClass := testing.MakeWorkloadPriorityClass(lowPriority).PriorityValue(100).Obj()
			util.MustCreate(ctx, k8sClient, lowPriorityClass)
			ginkgo.DeferCleanup(func() {
				gomega.Expect(k8sClient.Delete(ctx, lowPriorityClass)).To(gomega.Succeed())
			})

			midPriority := "mid-priority"
			midPriorityClass := testing.MakeWorkloadPriorityClass(midPriority).PriorityValue(200).Obj()
			util.MustCreate(ctx, k8sClient, midPriorityClass)
			ginkgo.DeferCleanup(func() {
				gomega.Expect(k8sClient.Delete(ctx, midPriorityClass)).To(gomega.Succeed())
			})

			highPriority := "high-priority"
			highPriorityClass := testing.MakeWorkloadPriorityClass(highPriority).PriorityValue(300).Obj()
			util.MustCreate(ctx, k8sClient, highPriorityClass)
			ginkgo.DeferCleanup(func() {
				gomega.Expect(k8sClient.Delete(ctx, highPriorityClass)).To(gomega.Succeed())
			})

			ginkgo.By("Create job-one with mid priority", func() {
				sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
					WorkloadPriorityClass(midPriority).
					Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
					NodeSelector("instance-type", "on-demand").
					Obj()
				util.MustCreate(ctx, k8sClient, sampleJob)
			})

			ginkgo.By("Verify the job-one is running", func() {
				util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
					"instance-type": "on-demand",
				})
			})

			ginkgo.By("Verify priority label is immutable when running", func() {
				createdJob := &batchv1.Job{}
				jobKey = client.ObjectKeyFromObject(sampleJob)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					createdJob.Labels[constants.WorkloadPriorityClassLabel] = ""
					g.Expect(k8sClient.Update(ctx, createdJob)).Should(testing.BeForbiddenError())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			var sampleJob2 *batchv1.Job
			ginkgo.By("Create job-two with low priority", func() {
				sampleJob2 = testingjob.MakeJob("test-job-2", ns.Name).
					Queue("main").
					RequestAndLimit(corev1.ResourceCPU, "1").
					RequestAndLimit(corev1.ResourceMemory, "20Mi").
					WorkloadPriorityClass(lowPriority).
					Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
					NodeSelector("instance-type", "on-demand").
					Obj()
				util.MustCreate(ctx, k8sClient, sampleJob2)
			})

			ginkgo.By("Verify workload with low priority is not admitted", func() {
				createdJob := &batchv1.Job{}
				jobKey = client.ObjectKeyFromObject(sampleJob2)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					g.Expect(*createdJob.Spec.Suspend).Should(gomega.BeTrue())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())

				wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob2.Name, sampleJob2.UID), Namespace: ns.Name}
				createdWorkload := &kueue.Workload{}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeFalse())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Increase job-two priority", func() {
				createdJob := &batchv1.Job{}
				jobKey = client.ObjectKeyFromObject(sampleJob2)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					createdJob.Labels[constants.WorkloadPriorityClassLabel] = highPriority
					g.Expect(k8sClient.Update(ctx, createdJob)).Should(gomega.Succeed())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Verify workload priority was updated", func() {
				wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob2.Name, sampleJob2.UID), Namespace: ns.Name}
				createdWorkload := &kueue.Workload{}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(ptr.Deref(createdWorkload.Spec.Priority, -1)).Should(gomega.Equal(highPriorityClass.Value))
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Verify job-two is running", func() {
				createdJob := &batchv1.Job{}
				jobKey = client.ObjectKeyFromObject(sampleJob2)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					g.Expect(*createdJob.Spec.Suspend).Should(gomega.BeFalse())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("Should not allow removing the workload's priority through the job", func() {
			samplePriority := "sample-priority"
			samplePriorityClass := testing.MakeWorkloadPriorityClass(samplePriority).PriorityValue(100).Obj()
			util.MustCreate(ctx, k8sClient, samplePriorityClass)
			ginkgo.DeferCleanup(func() {
				gomega.Expect(k8sClient.Delete(ctx, samplePriorityClass)).To(gomega.Succeed())
			})

			// Request more resources than are available to keep the job suspended
			ginkgo.By("Create job with priority", func() {
				sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
					WorkloadPriorityClass(samplePriority).
					Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
					NodeSelector("instance-type", "on-demand").
					RequestAndLimit(corev1.ResourceCPU, "2").
					Obj()
				util.MustCreate(ctx, k8sClient, sampleJob)
			})

			ginkgo.By("Verify workload is created and not admitted", func() {
				createdJob := &batchv1.Job{}
				jobKey = client.ObjectKeyFromObject(sampleJob)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					g.Expect(*createdJob.Spec.Suspend).Should(gomega.BeTrue())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())

				wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
				createdWorkload := &kueue.Workload{}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(createdWorkload.Spec.PriorityClassName).Should(gomega.Equal(samplePriority))
					g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeFalse())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("Remove job priority", func() {
				createdJob := &batchv1.Job{}
				jobKey = client.ObjectKeyFromObject(sampleJob)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					createdJob.Labels[constants.WorkloadPriorityClassLabel] = ""
					g.Expect(k8sClient.Update(ctx, createdJob)).Should(testing.BeForbiddenError())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})
		})
	})

	ginkgo.When("Creating a Job In a Twostepadmission Queue", func() {
		var (
			onDemandRF   *kueue.ResourceFlavor
			localQueue   *kueue.LocalQueue
			clusterQueue *kueue.ClusterQueue
			check        *kueue.AdmissionCheck
		)
		ginkgo.BeforeEach(func() {
			check = testing.MakeAdmissionCheck("check1").ControllerName("ac-controller").Obj()
			util.MustCreate(ctx, k8sClient, check)
			util.SetAdmissionCheckActive(ctx, k8sClient, check, metav1.ConditionTrue)
			onDemandRF = testing.MakeResourceFlavor("on-demand").
				NodeLabel("instance-type", "on-demand").Obj()
			util.MustCreate(ctx, k8sClient, onDemandRF)
			clusterQueue = testing.MakeClusterQueue("cluster-queue").
				ResourceGroup(
					*testing.MakeFlavorQuotas("on-demand").
						Resource(corev1.ResourceCPU, "1").
						Resource(corev1.ResourceMemory, "1Gi").
						Obj(),
				).
				AdmissionChecks("check1").
				Obj()
			util.MustCreate(ctx, k8sClient, clusterQueue)
			localQueue = testing.MakeLocalQueue("main", ns.Name).ClusterQueue("cluster-queue").Obj()
			util.MustCreate(ctx, k8sClient, localQueue)
		})
		ginkgo.AfterEach(func() {
			gomega.Expect(util.DeleteAllJobsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			// Force remove workloads to be sure that cluster queue can be removed.
			gomega.Expect(util.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			gomega.Expect(util.DeleteObject(ctx, k8sClient, localQueue)).Should(gomega.Succeed())
			util.ExpectObjectToBeDeleted(ctx, k8sClient, clusterQueue, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, onDemandRF, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, check, true)
			util.ExpectAllPodsInNamespaceDeleted(ctx, k8sClient, ns)
		})

		ginkgo.It("Should unsuspend a job only after all checks are cleared", func() {
			// Use a binary that ends.
			sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).Image(util.GetAgnHostImage(), util.BehaviorExitFast).Obj()
			util.MustCreate(ctx, k8sClient, sampleJob)

			createdWorkload := &kueue.Workload{}
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}

			ginkgo.By("verify the check is added to the workload", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(slices.ToMap(createdWorkload.Status.AdmissionChecks, func(i int) (kueue.AdmissionCheckReference, string) {
						return createdWorkload.Status.AdmissionChecks[i].Name, ""
					})).Should(gomega.BeComparableTo(map[kueue.AdmissionCheckReference]string{"check1": ""}))
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("waiting for the workload to be assigned", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadQuotaReserved))
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("checking the job remains suspended", func() {
				createdJob := &batchv1.Job{}
				jobKey := client.ObjectKeyFromObject(sampleJob)
				gomega.Consistently(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					g.Expect(createdJob.Spec.Suspend).Should(gomega.Equal(ptr.To(true)))
				}, util.ConsistentDuration, util.ShortInterval).Should(gomega.Succeed())
			})

			ginkgo.By("setting the check as successful", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					patch := util.BaseSSAWorkload(createdWorkload)
					workload.SetAdmissionCheckState(&patch.Status.AdmissionChecks, kueue.AdmissionCheckState{
						Name:  "check1",
						State: kueue.CheckStateReady,
					}, realClock)
					g.Expect(k8sClient.Status().Patch(ctx, patch, client.Apply, client.FieldOwner("test-admission-check-controller"), client.ForceOwnership)).Should(gomega.Succeed())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			// The job might have finished at this point. That shouldn't be a problem for the purpose of this test
			util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
				"instance-type": "on-demand",
			})
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
				g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeTrue())
				g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadFinished))
			}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.It("Should suspend a job when its checks become invalid", func() {
			util.MustCreate(ctx, k8sClient, sampleJob)

			createdWorkload := &kueue.Workload{}
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}

			ginkgo.By("verify the check is added to the workload", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(slices.ToMap(createdWorkload.Status.AdmissionChecks, func(i int) (kueue.AdmissionCheckReference, string) {
						return createdWorkload.Status.AdmissionChecks[i].Name, ""
					})).Should(gomega.BeComparableTo(map[kueue.AdmissionCheckReference]string{"check1": ""}))
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("setting the check as successful", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					patch := util.BaseSSAWorkload(createdWorkload)
					workload.SetAdmissionCheckState(&patch.Status.AdmissionChecks, kueue.AdmissionCheckState{
						Name:  "check1",
						State: kueue.CheckStateReady,
					}, realClock)
					g.Expect(k8sClient.Status().Patch(ctx, patch, client.Apply, client.FieldOwner("test-admission-check-controller"), client.ForceOwnership)).Should(gomega.Succeed())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			util.ExpectJobUnsuspendedWithNodeSelectors(ctx, k8sClient, jobKey, map[string]string{
				"instance-type": "on-demand",
			})

			ginkgo.By("setting the check as Rejected", func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					patch := util.BaseSSAWorkload(createdWorkload)
					workload.SetAdmissionCheckState(&patch.Status.AdmissionChecks, kueue.AdmissionCheckState{
						Name:  "check1",
						State: kueue.CheckStateRejected,
					}, realClock)
					g.Expect(k8sClient.Status().Patch(ctx, patch, client.Apply,
						client.FieldOwner("test-admission-check-controller"),
						client.ForceOwnership)).Should(gomega.Succeed())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})

			ginkgo.By("checking the job gets suspended", func() {
				createdJob := &batchv1.Job{}
				jobKey := client.ObjectKeyFromObject(sampleJob)
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, jobKey, createdJob)).Should(gomega.Succeed())
					g.Expect(ptr.Deref(createdJob.Spec.Suspend, false)).Should(gomega.BeTrue())
				}, util.Timeout, util.Interval).Should(gomega.Succeed())
			})
		})
	})
})

func expectJobUnsuspended(key types.NamespacedName) {
	job := &batchv1.Job{}
	gomega.EventuallyWithOffset(1, func(g gomega.Gomega) {
		g.Expect(k8sClient.Get(ctx, key, job)).To(gomega.Succeed())
		g.Expect(job.Spec.Suspend).Should(gomega.Equal(ptr.To(false)))
	}, util.Timeout, util.Interval).Should(gomega.Succeed())
}

func defaultOwnerReferenceForJob(name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{
			APIVersion: "batch/v1",
			Kind:       "Job",
			Name:       name,
		},
	}
}
