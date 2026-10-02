package service

// 商品源上的商品图进 keel（spec §7.4「图片下载存进 uploads」）：一组图的 URL 和上次一样就不动（product 映射的
// extra.images_sha）；变了就整组下载、落进上传存储、登记成 uploads（上传者是 binding，00302）、替换商品图。
//
// 只下载 allowURL 放行的地址（生产：https://cdn.shopify.com/…），大小与类型同后台上传（MaxUploadBytes，
// jpeg / png / webp）。任何一张失败整组放弃（已落盘的删掉），商品图保持原样、记 Warn —— 图片不挡商品同步。
// 下载在事务之外：不为一次网络调用攥着连接。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

type channelImages struct {
	store    UploadStore
	client   *http.Client
	allowURL func(*url.URL) bool
}

// WithImages 接上商品图下载：store 是上传存储（与后台上传同一个实例），allowURL 决定哪些地址可以下载。
// 不接时商品源的图不进 keel。
func (s *ChannelService) WithImages(store UploadStore, client *http.Client, allowURL func(*url.URL) bool) *ChannelService {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	s.images = &channelImages{store: store, client: client, allowURL: allowURL}
	return s
}

// ShopifyCDN 是生产环境放行的图片地址：https 且主机是 cdn.shopify.com。
func ShopifyCDN(u *url.URL) bool { return u.Scheme == "https" && u.Hostname() == "cdn.shopify.com" }

type productLinkExtra struct {
	ImagesSHA string `json:"images_sha,omitempty"`
}

func imagesSHA(urls []string) string {
	h := sha256.Sum256([]byte(strings.Join(urls, "\n")))
	return hex.EncodeToString(h[:])
}

// syncImages 见文件头。返回错误只给调用方记日志。
func (s *ChannelService) syncImages(ctx context.Context, b repository.ChannelBinding, productID int64, externalID string, urls []string) error {
	if s.images == nil || s.images.store == nil {
		return nil
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	if len(urls) > maxProductImages {
		urls = urls[:maxProductImages]
	}
	sha := imagesSHA(urls)
	var link repository.ChannelItemLink
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		link, e = tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemProduct, externalID)
		return e
	}); err != nil {
		return err
	}
	var ex productLinkExtra
	_ = json.Unmarshal(link.Extra, &ex)
	if ex.ImagesSHA == sha {
		return nil
	}
	var got []downloaded
	discard := func() {
		for _, g := range got {
			_ = s.images.store.Remove(g.key)
		}
	}
	for _, raw := range urls {
		g, err := s.downloadImage(ctx, merchantID, raw)
		if err != nil {
			discard()
			return fmt.Errorf("商品图 %s：%w", raw, err)
		}
		got = append(got, g)
	}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		ids := make([]int64, 0, len(got))
		for _, g := range got {
			up, err := tx.CreateChannelUpload(ctx, repository.NewChannelUpload{BindingID: b.ID, Driver: s.images.store.Driver(),
				StorageKey: g.key, ContentType: g.mime, SizeBytes: g.size, SHA256: g.sum})
			if err != nil {
				return err
			}
			ids = append(ids, up.ID)
		}
		if _, err := tx.ReplaceProductImages(ctx, productID, ids); err != nil {
			return err
		}
		extra, _ := json.Marshal(productLinkExtra{ImagesSHA: sha})
		return tx.UpsertChannelItemLink(ctx, repository.ChannelItemLink{BindingID: b.ID, Kind: repository.ChannelItemProduct,
			KeelID: productID, ExternalID: externalID, Extra: extra})
	})
	if err != nil {
		discard()
	}
	return err
}

type downloaded struct {
	key, mime, sum string
	size           int64
}

func (s *ChannelService) downloadImage(ctx context.Context, merchantID int64, raw string) (downloaded, error) {
	u, err := url.Parse(raw)
	if err != nil || s.images.allowURL == nil || !s.images.allowURL(u) {
		return downloaded{}, errors.New("地址不在放行范围内")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return downloaded{}, err
	}
	resp, err := s.images.client.Do(req)
	if err != nil {
		return downloaded{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return downloaded{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	mime, ext, ok := uploadExtensionFor(resp.Header.Get("Content-Type"))
	if !ok {
		return downloaded{}, fmt.Errorf("类型 %q 不收（只收 jpeg / png / webp）", resp.Header.Get("Content-Type"))
	}
	key, size, sum, err := s.images.store.Put(merchantID, ext, resp.Body, MaxUploadBytes)
	if err != nil {
		return downloaded{}, err
	}
	return downloaded{key: key, mime: mime, sum: sum, size: size}, nil
}
