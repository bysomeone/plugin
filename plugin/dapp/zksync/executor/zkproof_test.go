package executor

//
//import (
//	"github.com/33cn/chain33/util"
//	zt "github.com/33cn/plugin/plugin/dapp/zksync/types"
//	"github.com/stretchr/testify/assert"
//	"testing"
//)
//
//func TestHistoryProof(t *testing.T) {
//	dir, statedb, localdb := util.CreateTestDB()
//
//	defer util.CloseTestDB(dir, statedb)
//
//	zkproof := &zt.ZkCommitProof{
//		BlockEnd: 260,
//		IndexEnd: 1,
//		OldTreeRoot: "0",
//		NewTreeRoot: "15275177226630979939067149484578443289216960078842399286363628315212402600089",
//		PublicInput: "0000000200598525ca4193cf4b93bec143791ab18448b4cbb493f9de4b4fe5465df75c43126392d9a7c1cb6159d24d4d7e472e3354db90afaa23d6988aad2fa5bc0dc0dd",
//		Proof: "e719783d3341baa2da6711b3a4b4c74409f9edaca5a680bd331fa00bfb987a0df025281ca167988adb9245ef80bb50a6ca1070a9be5f3ad6884d57f07d21ef7e05b08797100728d48c90b34336cad1a74620ae35fac36332e7424349c98e8767e67f9589d9bbadd16afe5246cd53c2bd033f9144b541a49bb2337e2f6cbad2d1",
//		PubDatas: []string{
//			"36029346774777984",
//			"203486033877090063876096",
//			"1101756919913185326139557",
//			"281977360200387566569521",
//			"313001033228003142448210",
//			"521184178680751588042226",
//			"1159414034813409815415826",
//			"390672044009",
//			"104793518034474101637216",
//			"701289187814322624600403",
//			"495278876179408167157785",
//			"183000415066381182529448",
//			"310339862010084108986058",
//			"436079971580216708671740",
//			"43577537968016599968",
//			"36029346774777920",
//			"755578637259143234191360",
//			"1101756919913185326172079",
//			"281977360200387566569521",
//			"15381586",
//			"36029346774777936",
//			"1038920626231321947013120",
//			"21337",
//			"36029346774778000",
//			"755578637259143234191360",
//			"34735",
//			"36029071896871040",
//			"1038920626231321947013120",
//			"1101756919913185326158681",
//			"281977360200387566569521",
//			"459249680178568307061842",
//			"141995439407441164308787",
//			"794771466661560247906107",
//			"598747768130",
//			"36029346774778048",
//			"64",
//			"149188373381120",
//			"36029346774777888",
//			"192",
//			"335385442300072462123008",
//			"455157992636827746649722",
//			"377776930370105814377684",
//			"464644186447645625271088",
//			"774595626279670106033660",
//			"907037021586462392687",
//			"0",
//			"0",
//			"0",
//			"0",
//			"0",
//		},
//		ProofId: 1,
//	}
//
//	proofTable := NewCommitProofTable(localdb)
//	err := proofTable.Add(zkproof)
//	assert.Equal(t, nil, err)
//	kvs, err := proofTable.Save()
//	for _, kv := range kvs {
//		err = localdb.Set(kv.GetKey(), kv.GetValue())
//		assert.Equal(t, nil, err)
//	}
//
//	proof, err := getAccountProofInHistory(localdb, 1, "15275177226630979939067149484578443289216960078842399286363628315212402600089")
// 	assert.Equal(t, nil, err)
//	t.Log(proof)
//}
