import { computed, reactive, ref, watch } from 'vue'
import { api, ApiError, errorMessage, type Image } from '@/api/client.ts'
import { toast } from '@/components/ui/toast'
import { useSession } from '@/composables/useSession'
import { useTags } from '@/composables/useTags'
import { fileVideoMime } from '@/utils/media'
import { formatBytes } from '@/utils/format'

// 上传队列是全局状态：切换页面不会丢失，退出登录或换账号时清空

export type UploadStatus = 'queued' | 'uploading' | 'done' | 'failed'

export interface UploadItem {
  id: number
  name: string
  size: number
  kind: 'image' | 'video'
  /** 本地文件；从 URL 导入的条目没有 */
  file?: File
  /** 图片的本地预览地址（object URL） */
  previewUrl: string
  status: UploadStatus
  progress: number
  /** 服务器还在处理（文件已发送完） */
  processing: boolean
  error: string
  /** 只有服务器返回的失败可以重试；格式、大小不符合要求的不能 */
  retryable: boolean
  duplicate: boolean
  image?: Image
  bucketId?: number
  tagIds: number[]
}

export interface UploadStorage {
  id: number
  name: string
  type: string
}

const DEFAULT_MAX_FILES = 10
const BUCKET_KEY = 'currentBucket'
const AUTO_START_KEY = 'upload-auto-start'

const options = reactive({
  loaded: false,
  videoEnabled: false,
  maxImageSize: 10 * 1024 * 1024,
  maxVideoSize: 100 * 1024 * 1024,
  /** 实际允许的图片与视频 MIME 类型 */
  allowedTypes: ['image/jpeg', 'image/png', 'image/gif', 'image/webp'] as string[],
  maxFiles: DEFAULT_MAX_FILES,
  defaultStorage: 0,
  storages: [] as UploadStorage[],
})

const readStorage = (key: string) => {
  try { return localStorage.getItem(key) } catch { return null }
}
const writeStorage = (key: string, value: string) => {
  try { localStorage.setItem(key, value) } catch { /* 不保存也不影响使用 */ }
}

const items = ref<UploadItem[]>([])
const bucketId = ref(Number(readStorage(BUCKET_KEY)) || 0)
const tagIds = ref<number[]>([])
const autoStart = ref(readStorage(AUTO_START_KEY) === '1')
/** 每完成一批加一，页面据此刷新最近上传 */
const completedBatches = ref(0)

watch(bucketId, value => { if (value) writeStorage(BUCKET_KEY, String(value)) })
// 标签被删除后，从上传标签里去掉
const { tags: allTags, loaded: tagsLoaded } = useTags()
watch(allTags, list => {
  if (!tagsLoaded.value) return
  const ids = new Set(list.map(tag => tag.id))
  if (tagIds.value.some(id => !ids.has(id))) tagIds.value = tagIds.value.filter(id => ids.has(id))
})
watch(autoStart, value => writeStorage(AUTO_START_KEY, value ? '1' : '0'))

let sequence = 0

const queued = computed(() => items.value.filter(item => item.status === 'queued'))
const uploading = computed(() => items.value.filter(item => item.status === 'uploading'))
const busy = computed(() => uploading.value.length > 0)
const hasPending = () => items.value.some(item => item.status === 'queued' || item.status === 'uploading')

/** 选中的存储不可用（停用、已满、失去授权）时回到默认存储 */
const ensureBucket = () => {
  const ids = options.storages.map(storage => storage.id)
  if (ids.includes(bucketId.value)) return
  bucketId.value = ids.includes(options.defaultStorage) ? options.defaultStorage : ids[0] ?? 0
}

let optionsLoading: Promise<void> | null = null

/** 读取 /upload-options：可用存储、允许的类型和大小上限；force 时重新请求 */
export const loadUploadOptions = (force = false) => {
  if (force) optionsLoading = null
  optionsLoading ??= api.uploadOptions()
    .then(data => {
      const storages = Array.isArray(data.storage_buckets) ? data.storage_buckets as Record<string, unknown>[] : []
      const allowed = Array.isArray(data.allowed_types) ? (data.allowed_types as unknown[]).map(type => String(type).trim().toLowerCase()).filter(Boolean) : []
      Object.assign(options, {
        loaded: true,
        videoEnabled: data.video_upload_enabled === true,
        maxImageSize: Number(data.max_file_size) > 0 ? Number(data.max_file_size) : options.maxImageSize,
        maxVideoSize: Number(data.max_video_file_size) > 0 ? Number(data.max_video_file_size) : options.maxVideoSize,
        allowedTypes: allowed.length ? allowed : options.allowedTypes,
        maxFiles: Number(data.max_files) > 0 ? Number(data.max_files) : DEFAULT_MAX_FILES,
        defaultStorage: Number(data.default_storage) || 0,
        storages: storages.map(storage => ({ id: Number(storage.id), name: String(storage.name ?? ''), type: String(storage.type ?? '') })),
      })
      ensureBucket()
    })
    .catch(error => {
      optionsLoading = null
      throw error
    })
  return optionsLoading
}

const fingerprint = (file: File) => [file.name, file.size, file.lastModified, file.type].join(':')

const rejectReason = (file: File, kind: UploadItem['kind'], mime: string) => {
  if (kind === 'video') {
    if (!options.videoEnabled) return '站点未开启视频上传'
    if (file.size > options.maxVideoSize) return `超过视频 ${formatBytes(options.maxVideoSize)} 上限`
    return ''
  }
  if (mime === 'image/svg+xml') return '不支持 SVG（可能包含脚本）'
  if (!mime.startsWith('image/')) return '仅支持图片和 MP4 / WebM 视频'
  if (!options.allowedTypes.includes(mime)) return `不允许的类型 ${mime}`
  if (file.size > options.maxImageSize) return `超过图片 ${formatBytes(options.maxImageSize)} 上限`
  return ''
}

const release = (item: UploadItem) => {
  if (item.previewUrl) URL.revokeObjectURL(item.previewUrl)
}

/**
 * 把文件加入待上传列表：同一批里重复的文件跳过，超过数量上限的忽略；
 * 格式或大小不符合要求的也列出来，标明原因。开启「添加后立即上传」时直接开始。
 */
export const addFiles = async (input: FileList | File[]) => {
  const files = [...input]
  if (!files.length) return
  if (!options.loaded) await loadUploadOptions().catch(() => undefined)

  const pending = new Set(items.value.filter(item => item.file && (item.status === 'queued' || item.status === 'uploading')).map(item => fingerprint(item.file!)))
  let room = options.maxFiles - queued.value.length
  let duplicates = 0
  let overflow = 0
  let added = 0

  for (const file of files) {
    const key = fingerprint(file)
    if (pending.has(key)) { duplicates += 1; continue }
    const videoMime = fileVideoMime(file)
    const kind = videoMime ? 'video' : 'image'
    const reason = rejectReason(file, kind, (videoMime || file.type).toLowerCase())
    if (!reason && room <= 0) { overflow += 1; continue }
    pending.add(key)
    if (!reason) room -= 1
    items.value.push({
      id: ++sequence,
      name: file.name,
      size: file.size,
      kind,
      file: reason ? undefined : file,
      previewUrl: kind === 'image' && !reason ? URL.createObjectURL(file) : '',
      status: reason ? 'failed' : 'queued',
      progress: 0,
      processing: false,
      error: reason,
      retryable: false,
      duplicate: false,
      tagIds: [],
    })
    added += 1
  }

  if (duplicates) toast.info(`已跳过 ${duplicates} 个列表里已有的文件`)
  if (overflow) toast.error(`待上传列表最多 ${options.maxFiles} 个文件，已忽略 ${overflow} 个`)
  if (added && autoStart.value) void startUpload()
  return added
}

const summarize = (batch: UploadItem[]) => {
  const duplicates = batch.filter(item => item.status === 'done' && item.duplicate).length
  const succeeded = batch.filter(item => item.status === 'done').length - duplicates
  const failed = batch.filter(item => item.status === 'failed').length
  if (failed) toast.error(`上传完成：新增 ${succeeded}，已存在 ${duplicates}，失败 ${failed}`)
  else if (duplicates) toast.info(`上传完成：新增 ${succeeded}，已存在 ${duplicates}`)
  else if (succeeded) toast.success(`已上传 ${succeeded} 个文件`)
}

/** 上传所有待上传的文件；后端一次最多接收 max_files 个，正好是列表上限 */
export const startUpload = async () => {
  const batch = queued.value.filter(item => item.file)
  if (!batch.length || busy.value) return
  const target = bucketId.value
  const tags = [...tagIds.value]

  const form = new FormData()
  for (const item of batch) {
    Object.assign(item, { status: 'uploading', progress: 0, processing: false, error: '', retryable: false, bucketId: target, tagIds: tags })
    form.append('images', item.file!)
  }
  tags.forEach(tag => form.append('tag_ids', String(tag)))
  if (target) form.append('bucket_id', String(target))

  try {
    const result = await api.uploadImages(form, value => {
      for (const item of batch) {
        item.progress = value
        item.processing = value >= 100
      }
    })
    const files = Array.isArray(result?.files) ? result.files : []
    batch.forEach((item, index) => {
      const file = files[index]
      if (file?.success && file.image) {
        Object.assign(item, { status: 'done', progress: 100, processing: false, duplicate: !!file.duplicate, image: file.image })
        release(item)
        item.previewUrl = ''
        item.file = undefined
      } else {
        Object.assign(item, { status: 'failed', processing: false, error: file?.error?.detail || '上传失败', retryable: true })
      }
    })
    completedBatches.value += 1
    summarize(batch)
  } catch (error) {
    const detail = errorMessage(error, '上传失败')
    batch.forEach(item => Object.assign(item, { status: 'failed', processing: false, error: detail, retryable: true }))
    toast.error(`上传失败：${detail}`)
    // 整批被拒绝时可能是存储刚被停用、已满或失去授权，刷新可选存储
    if (error instanceof ApiError) void loadUploadOptions(true).catch(() => undefined)
  }
}

/** 从 URL 导入图片：直接请求服务器，结果显示在同一个列表里 */
export const importFromUrl = async (url: string) => {
  const name = (() => {
    try { return decodeURIComponent(new URL(url).pathname.split('/').filter(Boolean).pop() || '') || url } catch { return url }
  })()
  const item = reactive<UploadItem>({
    id: ++sequence, name, size: 0, kind: 'image', previewUrl: '', status: 'uploading', progress: 100, processing: true,
    error: '', retryable: false, duplicate: false, bucketId: bucketId.value, tagIds: [...tagIds.value],
  })
  items.value.push(item)
  try {
    const image = await api.importImage({ url, bucket_id: item.bucketId || undefined, tag_ids: item.tagIds })
    Object.assign(item, { status: 'done', processing: false, image, size: image.original_file_size || image.file_size })
    completedBatches.value += 1
    toast.success('已导入图片')
    return true
  } catch (error) {
    const detail = errorMessage(error, '导入失败')
    Object.assign(item, { status: 'failed', processing: false, error: detail })
    toast.error(`导入失败：${detail}`)
    if (error instanceof ApiError && error.problem?.status === 409) void loadUploadOptions(true).catch(() => undefined)
    return false
  }
}

/** 服务器返回失败的文件放回待上传列表 */
export const retryItem = (item: UploadItem) => {
  if (!item.retryable || !item.file || busy.value) return
  if (queued.value.length >= options.maxFiles) {
    toast.error(`待上传列表最多 ${options.maxFiles} 个文件`)
    return
  }
  Object.assign(item, { status: 'queued', error: '', retryable: false, progress: 0 })
}

export const removeItem = (item: UploadItem) => {
  if (item.status === 'uploading') return
  release(item)
  items.value = items.value.filter(candidate => candidate.id !== item.id)
}

/** 清空列表；只移除列表，不删除已上传的文件，正在上传的保留 */
export const clearItems = () => {
  items.value.filter(item => item.status !== 'uploading').forEach(release)
  items.value = items.value.filter(item => item.status === 'uploading')
}

export const hasPendingUploads = hasPending

const reset = () => {
  items.value.forEach(release)
  items.value = []
  tagIds.value = []
}

// 换账号或退出登录后不保留上一个账号的队列和标签选择
const { user } = useSession()
watch(() => user.value.id, (value, old) => { if (value !== old) reset() })

window.addEventListener('beforeunload', event => {
  if (!hasPending()) return
  event.preventDefault()
  event.returnValue = ''
})

export const useUploadQueue = () => ({
  options,
  items,
  bucketId,
  tagIds,
  autoStart,
  queued,
  uploading,
  busy,
  completedBatches,
})
