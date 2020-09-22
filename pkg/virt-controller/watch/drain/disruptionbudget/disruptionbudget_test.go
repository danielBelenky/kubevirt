package disruptionbudget_test

import (
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	v12 "k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	framework "k8s.io/client-go/tools/cache/testing"
	"k8s.io/client-go/tools/record"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/virt-controller/watch/drain/disruptionbudget"
)

var _ = Describe("Disruptionbudget", func() {

	var ctrl *gomock.Controller
	var stop chan struct{}
	var virtClient *kubecli.MockKubevirtClient
	var vmiInterface *kubecli.MockVirtualMachineInstanceInterface
	var vmiSource *framework.FakeControllerSource
	var vmiInformer cache.SharedIndexInformer
	var pdbInformer cache.SharedIndexInformer
	var pdbSource *framework.FakeControllerSource
	var recorder *record.FakeRecorder
	var mockQueue *testutils.MockWorkQueue
	var kubeClient *fake.Clientset
	var pdbFeeder *testutils.PodDisruptionBudgetFeeder
	var vmiFeeder *testutils.VirtualMachineFeeder

	var controller *disruptionbudget.DisruptionBudgetController

	syncCaches := func(stop chan struct{}) {
		go vmiInformer.Run(stop)
		go pdbInformer.Run(stop)

		Expect(cache.WaitForCacheSync(stop,
			vmiInformer.HasSynced,
			pdbInformer.HasSynced,
		)).To(BeTrue())
	}

	addVirtualMachine := func(vmi *v1.VirtualMachineInstance) {
		mockQueue.ExpectAdds(1)
		vmiSource.Add(vmi)
		mockQueue.Wait()
	}

	shouldExpectPDBDeletion := func(pdb *v1beta1.PodDisruptionBudget) {
		// Expect pod deletion
		kubeClient.Fake.PrependReactor("delete", "poddisruptionbudgets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			update, ok := action.(testing.DeleteAction)
			Expect(ok).To(BeTrue())
			Expect(pdb.Namespace).To(Equal(update.GetNamespace()))
			Expect(pdb.Name).To(Equal(update.GetName()))
			return true, nil, nil
		})
	}

	shouldExpectPDBCreation := func(uid types.UID, minAvailable int) {
		// Expect pod creation
		kubeClient.Fake.PrependReactor("create", "poddisruptionbudgets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			update, ok := action.(testing.CreateAction)
			pdb := update.GetObject().(*v1beta1.PodDisruptionBudget)
			Expect(ok).To(BeTrue())
			Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(minAvailable)))
			Expect(update.GetObject().(*v1beta1.PodDisruptionBudget).Spec.Selector.MatchLabels[v1.CreatedByLabel]).To(Equal(string(uid)))
			return true, update.GetObject(), nil
		})
	}

	BeforeEach(func() {
		stop = make(chan struct{})
		ctrl = gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(ctrl)

		vmiInformer, vmiSource = testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
		pdbInformer, pdbSource = testutils.NewFakeInformerFor(&v1beta1.PodDisruptionBudget{})
		recorder = record.NewFakeRecorder(100)

		controller = disruptionbudget.NewDisruptionBudgetController(vmiInformer, pdbInformer, recorder, virtClient)
		mockQueue = testutils.NewMockWorkQueue(controller.Queue)
		controller.Queue = mockQueue
		pdbFeeder = testutils.NewPodDisruptionBudgetFeeder(mockQueue, pdbSource)
		vmiFeeder = testutils.NewVirtualMachineFeeder(mockQueue, vmiSource)

		// Set up mock client
		virtClient.EXPECT().VirtualMachineInstance(v12.NamespaceDefault).Return(vmiInterface).AnyTimes()
		kubeClient = fake.NewSimpleClientset()
		virtClient.EXPECT().CoreV1().Return(kubeClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().PolicyV1beta1().Return(kubeClient.PolicyV1beta1()).AnyTimes()

		// Make sure that all unexpected calls to kubeClient will fail
		kubeClient.Fake.PrependReactor("*", "*", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			Expect(action).To(BeNil())
			return true, nil, nil
		})
		syncCaches(stop)

	})

	Context("A VirtualMachineInstance given which does not want to live-migrate on evictions", func() {

		It("should do nothing, if no pdb exists", func() {
			addVirtualMachine(newVirtualMachine("testvm"))
			controller.Execute()
		})

		It("should remove the pdb, if it is added to the cache", func() {
			vmi := newVirtualMachine("testvm")
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)

			shouldExpectPDBDeletion(pdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)
		})
	})

	Context("A VirtualMachineInstance given which wants to live-migrate on evictions", func() {

		It("should do nothing, if a pdb exists", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)

			controller.Execute()
		})

		It("should remove the pdb if the VMI disappears", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)

			controller.Execute()

			vmiFeeder.Delete(vmi)
			shouldExpectPDBDeletion(pdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)
		})

		It("should recreate the PDB if the VMI is recreated", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)

			controller.Execute()

			vmiFeeder.Delete(vmi)
			shouldExpectPDBDeletion(pdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)

			pdbFeeder.Delete(pdb)
			vmi.UID = "45356"
			vmiFeeder.Add(vmi)
			shouldExpectPDBCreation(vmi.UID, 1)
			controller.Execute()

			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulCreatePodDisruptionBudgetReason)
		})

		It("should delete a PDB which belongs to an old VMI", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)
			// new UID means new VMI
			vmi.UID = "changed"
			addVirtualMachine(vmi)

			shouldExpectPDBDeletion(pdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)
		})

		It("should not create a PDB for VMIs which are already marked for deletion", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			now := v13.Now()
			vmi.DeletionTimestamp = &now
			addVirtualMachine(vmi)

			controller.Execute()

			vmiFeeder.Delete(vmi)
			controller.Execute()
		})

		It("should remove the pdb if the VMI does not want to be migrated anymore", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)

			controller.Execute()

			vmi.Spec.EvictionStrategy = nil
			vmiFeeder.Modify(vmi)
			shouldExpectPDBDeletion(pdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)
		})

		It("should add the pdb, if it does not exist", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)

			shouldExpectPDBCreation(vmi.UID, 1)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulCreatePodDisruptionBudgetReason)
		})

		It("should recreate the pdb, if it disappears", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)
			controller.Execute()

			shouldExpectPDBCreation(vmi.UID, 1)
			pdbFeeder.Delete(pdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulCreatePodDisruptionBudgetReason)
		})

		It("should recreate the pdb, if the pdb is orphaned", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)
			controller.Execute()

			shouldExpectPDBCreation(vmi.UID, 1)
			newPdb := pdb.DeepCopy()
			newPdb.OwnerReferences = nil
			pdbFeeder.Modify(newPdb)
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulCreatePodDisruptionBudgetReason)
		})

		It("should re-create the pdb if a migration is pending", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			vmi.Status.MigrationState = newPendingMigration()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 1)
			pdbFeeder.Add(pdb)

			shouldExpectPDBCreation(vmi.UID, 2)
			shouldExpectPDBDeletion(pdb)
			vmiInterface.EXPECT().Update(gomock.Any())
			controller.Execute()
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulCreatePodDisruptionBudgetReason)
			testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)
		})

		It("should not re-create the pdb if the correct value is already set", func() {
			vmi := newVirtualMachine("testvm")
			vmi.Spec.EvictionStrategy = newEvictionStrategy()
			vmi.Status.MigrationState = newPendingMigration()
			addVirtualMachine(vmi)
			pdb := newPodDisruptionBudget(vmi, 2)
			pdbFeeder.Add(pdb)

			controller.Execute()
		})

		table.DescribeTable("should re-create the pdb if a migration is completed or failed",
			func(migrationState *v1.VirtualMachineInstanceMigrationState) {
				vmi := newVirtualMachine("testvm")
				vmi.Spec.EvictionStrategy = newEvictionStrategy()
				vmi.Status.Conditions = []v1.VirtualMachineInstanceCondition{
					{
						Type:   v1.VirtualMachineInstanceMigrationIsProtected,
						Status: v12.ConditionTrue,
					},
				}
				vmi.Status.MigrationState = newCompletedMigrationState()
				addVirtualMachine(vmi)
				pdb := newPodDisruptionBudget(vmi, 2)
				pdbFeeder.Add(pdb)

				shouldExpectPDBCreation(vmi.UID, 1)
				shouldExpectPDBDeletion(pdb)
				vmiInterface.EXPECT().Update(gomock.Any())
				controller.Execute()
				testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulCreatePodDisruptionBudgetReason)
				testutils.ExpectEvent(recorder, disruptionbudget.SuccessfulDeletePodDisruptionBudgetReason)
			},
			table.Entry("migration completed", newCompletedMigrationState()),
			table.Entry("migration failed", newFailedMigrationState()),
		)
	})

	AfterEach(func() {
		close(stop)
		// Ensure that we add checks for expected events to every test
		Expect(recorder.Events).To(BeEmpty())
		ctrl.Finish()
	})
})

func newVirtualMachine(name string) *v1.VirtualMachineInstance {
	vmi := v1.NewMinimalVMI("testvm")
	vmi.Namespace = v12.NamespaceDefault
	vmi.UID = "1234"
	return vmi
}

func newPodDisruptionBudget(vmi *v1.VirtualMachineInstance, minAvailable int) *v1beta1.PodDisruptionBudget {
	ma := intstr.FromInt(minAvailable)
	return &v1beta1.PodDisruptionBudget{
		ObjectMeta: v13.ObjectMeta{
			OwnerReferences: []v13.OwnerReference{
				*v13.NewControllerRef(vmi, v1.VirtualMachineInstanceGroupVersionKind),
			},
			GenerateName: "kubevirt-disruption-budget-",
			Namespace:    vmi.Namespace,
		},
		Spec: v1beta1.PodDisruptionBudgetSpec{
			MinAvailable: &ma,
			Selector: &v13.LabelSelector{
				MatchLabels: map[string]string{
					v1.CreatedByLabel: string(vmi.UID),
				},
			},
		},
	}
}

func newEvictionStrategy() *v1.EvictionStrategy {
	strategy := v1.EvictionStrategyLiveMigrate
	return &strategy
}

func newPendingMigration() *v1.VirtualMachineInstanceMigrationState {
	return &v1.VirtualMachineInstanceMigrationState{
		Pending: true,
	}
}

func newCompletedMigrationState() *v1.VirtualMachineInstanceMigrationState {
	return &v1.VirtualMachineInstanceMigrationState{
		Completed: true,
		Failed:    false,
	}
}

func newFailedMigrationState() *v1.VirtualMachineInstanceMigrationState {
	return &v1.VirtualMachineInstanceMigrationState{
		Completed: false,
		Failed:    true,
	}
}
