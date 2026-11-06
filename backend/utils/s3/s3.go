package s3

import (
	"context"
	"fmt"

	"oneimg/backend/models"
	utilsBuckets "oneimg/backend/utils/buckets"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// 创建S3客户端
func NewS3Client(setting models.Settings, buckets models.Buckets) (*s3.Client, error) {
	var (
		endpoint  string
		bucket    string
		accessKey string
		secretKey string
		region    = "auto" // R2使用auto区域
	)

	switch buckets.Type {
	case "s3":
		storageConfig := utilsBuckets.ConvertToS3Bucket(buckets.Config)
		endpoint = storageConfig.S3Endpoint
		bucket = storageConfig.S3Bucket
		accessKey = storageConfig.S3AccessKey
		secretKey = storageConfig.S3SecretKey
		region = "us-east-1"
	case "r2":
		storageConfig := utilsBuckets.ConvertToR2Bucket(buckets.Config)
		endpoint = storageConfig.R2Endpoint
		bucket = storageConfig.R2Bucket
		accessKey = storageConfig.R2AccessKey
		secretKey = storageConfig.R2SecretKey
	}

	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("S3/R2密钥为空")
	}
	if bucket == "" || endpoint == "" {
		return nil, fmt.Errorf("S3/R2配置缺失 [bucket:%s, endpoint:%s]", bucket, endpoint)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.TODO(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKey,
			secretKey,
			"", // Token
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置失败: %w", err)
	}

	// BaseEndpoint alone lets the SDK rewrite to bucket.host, which breaks MinIO and
	// IP endpoints; path-style keeps the configured host as the request host.
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	}), nil
}

func GetObject(client s3.Client, ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	return client.GetObject(ctx, input)
}

func DeleteObject(client s3.Client, ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	return client.DeleteObject(ctx, input)
}
