package commands

import (
	"fmt"
	"os"
	"time"

	"github.com/33cn/chain33/types"
	vty "github.com/33cn/plugin/plugin/dapp/vote/types"
	"github.com/spf13/cobra"
)

func createGroupCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "createGroup",
		Short:   "create tx(create vote group)",
		Run:     createGroup,
		Example: "createGroup -n=group1 -a=admin1 -m=member1 -m=member2",
	}
	createGroupFlags(cmd)
	return cmd
}

func createGroupFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("name", "n", "", "group name")
	cmd.Flags().StringArrayP("admins", "a", nil, "group admin address array, default set creator as admin")
	cmd.Flags().StringArrayP("members", "m", nil, "group member address array")
	cmd.Flags().UintSliceP("weights", "w", nil, "member vote weight array")
	markRequired(cmd, "name")
}

func createGroup(cmd *cobra.Command, args []string) {

	name, _ := cmd.Flags().GetString("name")
	admins, _ := cmd.Flags().GetStringArray("admins")
	memberAddrs, _ := cmd.Flags().GetStringArray("members")
	weights, _ := cmd.Flags().GetUintSlice("weights")

	if name == "" {
		fmt.Fprintf(os.Stderr, "ErrNilGroupName")
		return
	}
	if len(weights) == 0 {
		weights = make([]uint, len(memberAddrs))
	}
	if len(weights) != len(memberAddrs) {
		fmt.Fprintf(os.Stderr, "member address array length should equal with vote weight array length")
		return
	}

	members := make([]*vty.GroupMember, 0)
	for i, addr := range memberAddrs {
		members = append(members, &vty.GroupMember{Addr: addr, VoteWeight: uint32(weights[i])})
	}

	params := &vty.CreateGroup{
		Name:    name,
		Admins:  admins,
		Members: members,
	}
	sendCreateTxRPC(cmd, vty.NameCreateGroupAction, params)
}

func updateGroupCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "updateGroup",
		Short:   "create tx(update group members or admin)",
		Run:     updateGroup,
		Example: "updateGroup -g=id -a=addMember1 -a=addMember2 -r=removeMember1 ...",
	}
	updateGroupFlags(cmd)
	return cmd
}

func updateGroupFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("groupID", "g", "", "group id")
	cmd.Flags().StringArrayP("addMembers", "a", nil, "group member address array for adding")
	cmd.Flags().UintSliceP("weights", "w", nil, "member vote weight array for adding")
	cmd.Flags().StringArrayP("removeMembers", "r", nil, "group member address array for removing")
	cmd.Flags().StringArrayP("addAdmins", "d", nil, "group admin address array for adding")
	cmd.Flags().StringArrayP("removeAdmins", "m", nil, "group admin address array for removing")
	markRequired(cmd, "groupID")
}

func updateGroup(cmd *cobra.Command, args []string) {

	groupID, _ := cmd.Flags().GetString("groupID")
	addAddrs, _ := cmd.Flags().GetStringArray("addMembers")
	weights, _ := cmd.Flags().GetUintSlice("weights")
	removeAddrs, _ := cmd.Flags().GetStringArray("removeMembers")
	addAdmins, _ := cmd.Flags().GetStringArray("addAdmins")
	removeAdmins, _ := cmd.Flags().GetStringArray("removeAdmins")

	if groupID == "" {
		fmt.Fprintf(os.Stderr, "ErrNilGroupID")
		return
	}
	if len(weights) == 0 {
		weights = make([]uint, len(addAddrs))
	}
	if len(weights) != len(addAddrs) {
		fmt.Fprintf(os.Stderr, "member address array length should equal with vote weight array length")
		return
	}

	addMembers := make([]*vty.GroupMember, 0)
	for i, addr := range addAddrs {
		addMembers = append(addMembers, &vty.GroupMember{Addr: addr, VoteWeight: uint32(weights[i])})
	}

	params := &vty.UpdateGroup{
		GroupID:       groupID,
		RemoveMembers: removeAddrs,
		AddMembers:    addMembers,
		AddAdmins:     addAdmins,
		RemoveAdmins:  removeAdmins,
	}
	sendCreateTxRPC(cmd, vty.NameUpdateGroupAction, params)
}

func createVoteCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "createVote",
		Short: "create tx(create vote)",
		Run:   createVote,
	}
	createVoteFlags(cmd)
	return cmd
}

func createVoteFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("name", "n", "", "vote name")
	cmd.Flags().StringP("groupID", "g", "", "belonging group id")
	cmd.Flags().StringArrayP("options", "o", nil, "vote option array")
	cmd.Flags().Int64P("beginTime", "b", 0, "vote begin unix timestamp, default set current time")
	cmd.Flags().StringP("duration", "d", "24h", "vote duration time, such as(10s, 10m, 10h), default 24h")

	markRequired(cmd, "name", "groupID", "options")
}

func createVote(cmd *cobra.Command, args []string) {
	name, _ := cmd.Flags().GetString("name")
	groupID, _ := cmd.Flags().GetString("groupID")
	options, _ := cmd.Flags().GetStringArray("options")
	beginTime, _ := cmd.Flags().GetInt64("beginTime")
	duration, _ := cmd.Flags().GetString("duration")
	if name == "" {
		fmt.Fprintf(os.Stderr, "ErrNilVoteName")
		return
	}
	if len(groupID) == 0 {
		fmt.Fprintf(os.Stderr, "ErrNilGroupID")
		return
	}

	if len(options) == 0 {
		fmt.Fprintf(os.Stderr, "ErrNilOptions")
		return
	}
	if beginTime == 0 {
		beginTime = types.Now().Unix()
	}

	dt, err := time.ParseDuration(duration)
	if err != nil {
		fmt.Fprintf(os.Stderr, "InvalidDurationTime:"+err.Error())
		return
	}
	endTime := beginTime + int64(dt/time.Second)

	params := &vty.CreateVote{
		Name:           name,
		GroupID:        groupID,
		VoteOptions:    options,
		BeginTimestamp: beginTime,
		EndTimestamp:   endTime,
	}
	sendCreateTxRPC(cmd, vty.NameCreateVoteAction, params)
}

func commitVoteCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "commitVote",
		Short: "create tx(commit vote)",
		Run:   commitVote,
	}
	commitVoteFlags(cmd)
	return cmd
}

func commitVoteFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("voteID", "v", "", "vote id")
	cmd.Flags().Uint32P("optionIndex", "o", 0, "voting option index in option array")
	markRequired(cmd, "voteID", "optionIndex")
}

func commitVote(cmd *cobra.Command, args []string) {
	voteID, _ := cmd.Flags().GetString("voteID")
	optionIndex, _ := cmd.Flags().GetUint32("optionIndex")
	if voteID == "" {
		fmt.Fprintf(os.Stderr, "ErrNilVoteID")
		return
	}

	params := &vty.CommitVote{
		VoteID:      voteID,
		OptionIndex: optionIndex,
	}
	sendCreateTxRPC(cmd, vty.NameCommitVoteAction, params)
}

func closeVoteCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "closeVote",
		Short: "create tx(close vote)",
		Run:   closeVote,
	}
	closeVoteFlags(cmd)
	return cmd
}

func closeVoteFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("voteID", "v", "", "vote id")
	markRequired(cmd, "voteID")
}

func closeVote(cmd *cobra.Command, args []string) {
	voteID, _ := cmd.Flags().GetString("voteID")
	if voteID == "" {
		fmt.Fprintf(os.Stderr, "ErrNilVoteID")
		return
	}

	params := &vty.CloseVote{
		VoteID: voteID,
	}
	sendCreateTxRPC(cmd, vty.NameCloseVoteAction, params)
}

func updateMemberCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "updateMember",
		Short: "create tx(update member name)",
		Run:   updateMember,
	}
	updateMemberFlags(cmd)
	return cmd
}

func updateMemberFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("name", "n", "", "member name")
	markRequired(cmd, "name")
}

func updateMember(cmd *cobra.Command, args []string) {
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		fmt.Fprintf(os.Stderr, "ErrNilMemberName")
		return
	}

	params := &vty.UpdateMember{
		Name: name,
	}
	sendCreateTxRPC(cmd, vty.NameUpdateMemberAction, params)
}