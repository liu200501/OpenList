package _139

// 本文件实现"跨账号分享秒转存"：
// 账号 A 为源（创建分享），账号 B 为目标（订阅分享 + 转存到自己的云盘）。
// 全流程为云端服务端操作，不经过客户端上传/下载字节（抓包验证 taskStatus 秒级完成）。
//
// 抓包来源：4_Full.txt（账号 A 调用 IOutLink/getOutLink 建分享）、
//          4_Full2.txt（账号 B 调用 subscribe/outLink/subOutLink、
//          IBatchOprTask/createOuterLinkBatchOprTask、queryBatchOprTaskDetail）。

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	log "github.com/sirupsen/logrus"
)

const shareBaseURL = "https://share-kd-njs.yun.139.com/yun-share"

// sharePlainPost 调用不加密的分享 API（getOutLink / subOutLink 走明文 JSON）。
// 使用 d 自身的账号授权（d.getAuthorization() / d.getAccount()），
// 因此源、目标账号各用各的驱动实例调用即可。
func (d *Yun139) sharePlainPost(pathname string, data interface{}, resp interface{}) ([]byte, error) {
	url := shareBaseURL + pathname
	req := base.RestyClient.R().
		SetHeaders(d.shareHeaders()).
		SetHeader("Content-Type", "application/json;charset=UTF-8").
		SetBody(data)
	if resp != nil {
		req.SetResult(resp)
	}
	r, err := req.Post(url)
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("139 share request %s: status %d: %s", pathname, r.StatusCode(), r.String())
	}
	log.Debugf("[139Transfer] plain post %s resp: %s", pathname, r.String())
	return r.Body(), nil
}

// createOutLink 由源账号（d == 源驱动）为指定的文件夹/文件创建分享。
// srcFolderIDs 为源内容 ID（caID/coID），创建成功后返回 linkID + passwd。
type OutLinkInfo struct {
	LinkID string
	Passwd string
}

func (d *Yun139) createOutLink(srcFolderIDs []string, srcFileIDs []string, name string) (*OutLinkInfo, error) {
	reqBody := base.Json{
		"getOutLinkReq": base.Json{
			"linkType":     0,
			"subLinkType":  0,
			"encrypt":      1,
			"coIDLst":      base.Json{"item": srcFileIDs},
			"caIDLst":      base.Json{"item": srcFolderIDs},
			"pubType":      1,
			"dedicatedName": name,
			"periodUnit":    1,
			"viewerLst":     base.Json{},
			"userDomainId":  d.UserDomainID,
			"extInfo":       base.Json{"isWatermark": 0, "shareChannel": "3001"},
			"passwdAutoFill": 1,
			"commonAccountInfo": base.Json{
				"account":     d.getAccount(),
				"accountType": 1,
			},
		},
	}
	var resp CreateOutLinkResp
	if _, err := d.sharePlainPost("/richlifeApp/devapp/IOutLink/getOutLink", reqBody, &resp); err != nil {
		return nil, err
	}
	if !resp.Success && resp.ResultCode != "0" && resp.Code != "0" {
		return nil, fmt.Errorf("139 create out link failed: code=%s resultCode=%s desc=%s", resp.Code, resp.ResultCode, resp.Desc)
	}
	if len(resp.Data.GetOutLinkResSet) == 0 {
		return nil, fmt.Errorf("139 create out link: empty result")
	}
	item := resp.Data.GetOutLinkResSet[0]
	return &OutLinkInfo{LinkID: item.LinkID, Passwd: item.Passwd}, nil
}

// subOutLink 由目标账号（d == 目标驱动）订阅分享，使后续转存可用。
func (d *Yun139) subOutLink(linkID, passwd string) error {
	reqBody := base.Json{
		"linkId":       linkID,
		"passwd":       passwd,
		"userDomainId": d.UserDomainID,
	}
	// 响应 {"success":true,"code":"0000",...}，用 base.Json 接住即可
	var resp base.Json
	if _, err := d.sharePlainPost("/subscribe/outLink/subOutLink", reqBody, &resp); err != nil {
		return err
	}
	log.Debugf("[139Transfer] subOutLink resp: %v", resp)
	return nil
}

// createOuterLinkBatchOprTask 由目标账号发起转存任务，返回 taskID。
// 该接口 body 加密，复用 sharePost（yun139EncryptedRequest + shareAesKey）。
func (d *Yun139) createOuterLinkBatchOprTask(linkID string, srcFolderIDs, srcFileIDs []string, dstDirID, dstDirName string) (string, error) {
	// 用新切片，避免改动调用方传入的源 ID 列表。
	catalogList := make([]string, 0, len(srcFolderIDs))
	for _, id := range srcFolderIDs {
		// 抓包要点：catalogInfoList 里的源内容 ID 带前导 "/"（建分享时用裸 ID）。
		if id != "" && !strings.HasPrefix(id, "/") {
			id = "/" + id
		}
		catalogList = append(catalogList, id)
	}
	contentList := srcFileIDs
	if contentList == nil {
		contentList = []string{}
	}
	// 抓包要点：commonAccountInfo 位于顶层；taskInfo 内还需 newCatalogName。
	reqBody := base.Json{
		"createOuterLinkBatchOprTaskReq": base.Json{
			"msisdn":      d.getAccount(),
			"ownerAccount": "",
			"taskType":    1,
			"taskInfo": base.Json{
				"contentInfoList": contentList,
				"catalogInfoList": catalogList,
				"newCatalogID":    dstDirID,
				"newCatalogName":  dstDirName,
				"linkID":          linkID,
				"needPassword":    true,
			},
			"linkID":       linkID,
			"needPassword": true,
		},
		"commonAccountInfo": base.Json{
			"account":     d.getAccount(),
			"accountType": 1,
		},
	}
	var resp CreateTransferTaskResp
	if _, err := d.sharePost("/richlifeApp/devapp/IBatchOprTask/createOuterLinkBatchOprTask", reqBody, &resp); err != nil {
		return "", err
	}
	if !resp.Success && resp.ResultCode != "0" && resp.Code != "0" {
		return "", fmt.Errorf("139 create transfer task failed: code=%s resultCode=%s desc=%s", resp.Code, resp.ResultCode, resp.Desc)
	}
	if resp.Data.TaskID == "" {
		return "", fmt.Errorf("139 create transfer task: empty taskID")
	}
	return resp.Data.TaskID, nil
}

// queryBatchOprTaskDetail 查询转存任务状态，轮询直到完成或超时。
func (d *Yun139) queryBatchOprTaskDetail(taskID string) (*TransferTaskDetail, error) {
	reqBody := base.Json{
		"queryBatchOprTaskDetailReq": base.Json{
			"taskID":  taskID,
			"msisdn":  d.getAccount(),
			"commonAccountInfo": base.Json{
				"account":     d.getAccount(),
				"accountType": 1,
			},
		},
	}
	var resp TransferTaskDetail
	if _, err := d.sharePost("/richlifeApp/devapp/IBatchOprTask/queryBatchOprTaskDetail", reqBody, &resp); err != nil {
		return nil, err
	}
	if !resp.Success && resp.ResultCode != "0" && resp.Code != "0" {
		return nil, fmt.Errorf("139 query transfer task failed: code=%s resultCode=%s desc=%s", resp.Code, resp.ResultCode, resp.Desc)
	}
	return &resp, nil
}

// waitTransferTask 轮询转存任务直到完成。taskStatus: 1=进行中 2=完成；
// resultCode=0 表示成功。返回任务详情供调用方统计结果。
func (d *Yun139) waitTransferTask(taskID string, timeout time.Duration) (*TransferTaskDetail, error) {
	deadline := time.Now().Add(timeout)
	for {
		detail, err := d.queryBatchOprTaskDetail(taskID)
		if err != nil {
			return nil, err
		}
		task := detail.Data.BatchOprTask
		if task.TaskStatus == 2 {
			if task.ResultCode != 0 && task.TaskResultCode != 0 {
				return detail, fmt.Errorf("139 transfer task %s failed: resultCode=%d taskResultCode=%d", taskID, task.ResultCode, task.TaskResultCode)
			}
			return detail, nil
		}
		if time.Now().After(deadline) {
			return detail, fmt.Errorf("139 transfer task %s timeout (status=%d progress=%d)", taskID, task.TaskStatus, task.Progress)
		}
		time.Sleep(time.Second)
	}
}

// TransferShareResult 转存结果摘要。
type TransferShareResult struct {
	TaskID       string   `json:"task_id"`
	LinkID       string   `json:"link_id"`
	Passwd       string   `json:"passwd"`
	Status       string   `json:"status"`
	FolderCount  int      `json:"folder_count"`
	FileCount    int      `json:"file_count"`
	FolderIDs    []string `json:"folder_ids,omitempty"`
	FileIDs      []string `json:"file_ids,omitempty"`
}

// SaveShareToPath 跨账号秒转存的完整编排：
//   1. 源账号 src 创建分享（srcFolderIDs/srcFileIDs 为源账号里的内容 ID）
//   2. 目标账号 d 订阅该分享
//   3. 目标账号发起转存任务到 dstDirID（目标账号里的目录 ID）
//   4. 轮询任务直到完成，收集新生成的内容 ID
//
// src 与 d 必须是两个不同账号的 *Yun139 实例，各自持有自己的授权。
func (d *Yun139) SaveShareToPath(src *Yun139, srcFolderIDs, srcFileIDs []string, dstDirID, dstDirName string) (*TransferShareResult, error) {
	if src == nil {
		return nil, fmt.Errorf("139 transfer: source storage is nil")
	}
	if len(srcFolderIDs) == 0 && len(srcFileIDs) == 0 {
		return nil, fmt.Errorf("139 transfer: no source content specified")
	}
	if d.isShare() {
		return nil, fmt.Errorf("139 transfer: destination cannot be a share storage")
	}

	// 1) 源账号创建分享
	link, err := src.createOutLink(srcFolderIDs, srcFileIDs, "openlist-transfer")
	if err != nil {
		return nil, fmt.Errorf("139 transfer: create share on source failed: %w", err)
	}
	log.Infof("[139Transfer] created out link %s (passwd=%s) for %d folders / %d files",
		link.LinkID, link.Passwd, len(srcFolderIDs), len(srcFileIDs))

	// 2) 目标账号订阅分享
	if err := d.subOutLink(link.LinkID, link.Passwd); err != nil {
		return nil, fmt.Errorf("139 transfer: subscribe share on destination failed: %w", err)
	}

	// 3) 发起转存
	taskID, err := d.createOuterLinkBatchOprTask(link.LinkID, srcFolderIDs, srcFileIDs, dstDirID, dstDirName)
	if err != nil {
		return nil, fmt.Errorf("139 transfer: create transfer task failed: %w", err)
	}
	log.Infof("[139Transfer] transfer task %s submitted", taskID)

	// 4) 等待完成
	detail, err := d.waitTransferTask(taskID, 5*time.Minute)
	if err != nil {
		return nil, err
	}

	result := &TransferShareResult{
		TaskID: taskID,
		LinkID: link.LinkID,
		Passwd: link.Passwd,
		Status: "success",
	}
	// 抓包中每个条目的 reason=="0000" 表示该项转存成功，只统计成功项。
	contains := func(list []string, id string) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}
	for _, f := range detail.Data.CatalogList.IDRspInfo {
		if f.Reason == "0000" && !contains(result.FolderIDs, f.RstID) {
			result.FolderIDs = append(result.FolderIDs, f.RstID)
		}
	}
	for _, f := range detail.Data.ContentList.IDRspInfo {
		if f.Reason == "0000" && !contains(result.FileIDs, f.RstID) {
			result.FileIDs = append(result.FileIDs, f.RstID)
		}
	}
	result.FolderCount = len(result.FolderIDs)
	result.FileCount = len(result.FileIDs)
	return result, nil
}
