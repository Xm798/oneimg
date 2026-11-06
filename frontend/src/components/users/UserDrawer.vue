<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { api, ApiError, errorMessage, type ManagedUser } from '@/api/client.ts'
import { usernameError } from '@/components/auth/auth'
import StorageTypeIcon from '@/components/storage/StorageTypeIcon.vue'
import { toast, UiAvatar, UiButton, UiDrawer, UiField, UiInput, UiSegmented, UiSwitch } from '@/components/ui'
import { effectiveCodes, isSuperAdmin, ROLE_ADMIN, ROLE_USER } from '@/utils/permissions.ts'
import { ALL_CODES, PERMISSION_GROUPS } from './permissions'
import type { UserStorage } from './types'

const props = defineProps<{
  user: ManagedUser | null
  storages: UserStorage[]
  defaultStorage: number
  /** 存储列表不完整（当前管理员没有存储权限，只能读到可上传的存储） */
  storagesPartial: boolean
  canEditRole: boolean
  canEditPermissions: boolean
  /** The signed-in admin; only codes it holds can be granted. */
  actor: Record<string, unknown>
}>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ saved: [] }>()

const draft = reactive({ username: '', role: String(ROLE_USER), codes: [] as string[], buckets: [] as number[] })
const usernameMessage = ref('')
const saving = ref(false)

watch([open, () => props.user], () => {
  const user = props.user
  if (!open.value || !user) return
  Object.assign(draft, {
    username: user.username,
    role: String(user.role),
    // Stale codes on a plain user grant nothing, and the server clears them on promotion.
    codes: effectiveCodes(user),
    buckets: [...(user.permission?.bucket_ids ?? [])],
  })
  usernameMessage.value = ''
}, { immediate: true })

const isAdminDraft = computed(() => draft.role === String(ROLE_ADMIN))
const roleOptions = [
  { value: String(ROLE_USER), label: '普通用户' },
  { value: String(ROLE_ADMIN), label: '管理员' },
]

const heldCodes = computed(() => effectiveCodes(props.actor))
const holds = (code: string) => heldCodes.value.includes(code)
// An equal set makes the target a peer that only a higher admin can manage.
const becomesPeer = computed(() => isAdminDraft.value && !isSuperAdmin(props.actor) && draft.codes.length >= heldCodes.value.length)

const toggleCode = (code: string, on: boolean) => {
  if (!holds(code)) return
  draft.codes = on ? [...new Set([...draft.codes, code])] : draft.codes.filter(item => item !== code)
}
const heldItems = (codes: readonly { code: string }[]) => codes.map(item => item.code).filter(holds)
const groupAll = (codes: readonly { code: string }[]) => heldItems(codes).every(code => draft.codes.includes(code))
const toggleGroup = (codes: readonly { code: string }[]) => {
  const list = heldItems(codes)
  draft.codes = groupAll(codes) ? draft.codes.filter(code => !list.includes(code)) : [...new Set([...draft.codes, ...list])]
}

// 已授权、但当前列表里没有的存储（列表不完整时）也列出来，避免保存时被悄悄去掉
const storageRows = computed(() => {
  const known = new Set(props.storages.map(storage => storage.id))
  const extra = draft.buckets.filter(id => !known.has(id)).map(id => ({ id, name: `存储 #${id}`, type: '', disabled: false }))
  return [...props.storages, ...extra]
})
const toggleBucket = (id: number) => {
  if (id === props.defaultStorage || !props.canEditPermissions) return
  draft.buckets = draft.buckets.includes(id) ? draft.buckets.filter(item => item !== id) : [...draft.buckets, id]
}

const sameList = <T,>(a: T[], b: T[]) => a.length === b.length && a.every(item => b.includes(item))

const save = async () => {
  const user = props.user
  if (!user || saving.value) return
  usernameMessage.value = ''
  const name = draft.username.trim()
  const profile: { username?: string; role?: number } = {}
  if (props.canEditRole && name !== user.username) {
    const message = usernameError(name)
    if (message) { usernameMessage.value = message; return }
    profile.username = name
  }
  if (props.canEditRole && Number(draft.role) !== user.role) profile.role = Number(draft.role)
  const codes = isAdminDraft.value ? draft.codes : []
  const originalCodes = effectiveCodes(user)
  const permissionsChanged = props.canEditPermissions && (!sameList(codes, originalCodes) || !sameList(draft.buckets, user.permission?.bucket_ids ?? []))
  if (!Object.keys(profile).length && !permissionsChanged) {
    open.value = false
    return
  }

  saving.value = true
  let profileSaved = false
  try {
    // 先改用户名和角色，再保存权限；两步分开提交，第二步失败时说明第一步已经生效
    if (Object.keys(profile).length) {
      await api.updateUser(user.id, profile)
      profileSaved = true
    }
    if (permissionsChanged) await api.setUserPermissions(user.id, { codes, bucket_ids: draft.buckets })
    toast.success(`已保存 ${name || user.username} 的设置`)
    open.value = false
    emit('saved')
  } catch (error) {
    const message = errorMessage(error, '保存失败，请稍后重试')
    const code = error instanceof ApiError ? error.problem?.code : ''
    if (!profileSaved && (code === 'username_conflict' || message.includes('用户名'))) usernameMessage.value = message
    else if (profileSaved) {
      toast.error(`用户名和角色已保存，但权限没有保存：${message}`)
      emit('saved')
    } else {
      toast.error(message)
      const status = error instanceof ApiError ? error.problem?.status : 0
      if (status === 403 || status === 409) emit('saved')
    }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UiDrawer v-model:open="open" :title="user?.username ?? ''" description="角色与权限">
    <template #leading>
      <UiAvatar :name="user?.username" :src="user?.avatar_url" />
    </template>

    <form id="user-drawer-form" class="flex flex-col gap-5" novalidate @submit.prevent="save">
      <UiField label="用户名" for="user-drawer-name" :error="usernameMessage" :hint="canEditRole ? '3–50 个字符，用户下次登录时使用新用户名' : '需要「修改用户名和角色」权限'">
        <UiInput id="user-drawer-name" v-model="draft.username" maxlength="50" autocomplete="off" :disabled="!canEditRole" :invalid="!!usernameMessage" />
      </UiField>

      <UiField label="角色">
        <UiSegmented v-model="draft.role" :options="roleOptions" :disabled="!canEditRole" aria-label="角色" class="self-start" />
        <template #hint>
          {{ isAdminDraft ? '管理员可以使用全部存储，后台功能按下面勾选的权限开放。' : '普通用户只能管理自己的文件，没有后台功能权限。' }}
          <b v-if="user?.role === ROLE_ADMIN && !isAdminDraft" class="font-[550] text-oi-warn">改为普通用户会清空已有的功能权限。</b>
        </template>
      </UiField>

      <fieldset v-if="isAdminDraft" class="flex min-w-0 flex-col gap-2.5" :disabled="!canEditPermissions">
        <legend class="mb-1.5 text-[12.5px] font-[550] text-oi-text">
          功能权限 <span class="font-normal text-oi-muted">{{ draft.codes.length }} / {{ ALL_CODES.length }}</span>
          <span v-if="!canEditPermissions" class="ml-1 font-normal text-oi-muted">（需要「编辑权限」权限）</span>
        </legend>
        <div v-for="group in PERMISSION_GROUPS" :key="group.name" class="overflow-hidden rounded-[14px] border border-oi-line">
          <div class="flex items-center gap-2.5 border-b border-oi-line bg-oi-panel px-3.5 py-2 text-[13px] font-semibold text-oi-ink">
            <i :class="[group.icon, 'text-base text-oi-muted']" aria-hidden="true" />{{ group.name }}
            <UiButton v-if="canEditPermissions && heldItems(group.items).length" size="sm" variant="ghost" class="ml-auto !h-6" @click="toggleGroup(group.items)">
              {{ groupAll(group.items) ? '全不选' : '全选' }}<span class="sr-only">{{ group.name }}</span>
            </UiButton>
          </div>
          <label
            v-for="item in group.items"
            :key="item.code"
            class="flex cursor-pointer items-center justify-between gap-3 border-b border-oi-line px-3.5 py-2.5 text-[13px] last:border-b-0"
          >
            <span class="min-w-0">{{ item.label }}<code class="ml-1.5 font-oi-mono text-[11px] text-oi-faint">{{ item.code }}</code></span>
            <UiSwitch :model-value="draft.codes.includes(item.code)" :disabled="!canEditPermissions || !holds(item.code)" :aria-label="item.label" :title="holds(item.code) ? undefined : '你没有这项权限，不能授予'" @update:model-value="toggleCode(item.code, $event)" />
          </label>
        </div>
        <p v-if="canEditPermissions && becomesPeer" class="flex items-center gap-1.5 text-[12.5px] text-oi-warn" role="status">
          <i class="ri-error-warning-line" aria-hidden="true" />保存后你将无法再管理该用户
        </p>
      </fieldset>

      <div v-else class="flex flex-col gap-2">
        <span id="user-drawer-storages" class="text-[12.5px] font-[550] text-oi-text">可用存储</span>
        <p class="-mt-0.5 text-xs text-oi-muted">
          默认存储始终可用；已停用和配额已满的存储不会出现在上传选项中，授权会保留。
          <template v-if="storagesPartial">你没有存储管理权限，这里只列出当前可以上传的存储。</template>
        </p>
        <div class="overflow-hidden rounded-[14px] border border-oi-line" role="group" aria-labelledby="user-drawer-storages">
          <button
            v-for="storage in storageRows"
            :key="storage.id"
            type="button"
            role="checkbox"
            :aria-label="storage.id === defaultStorage ? `${storage.name}（默认存储，始终可用）` : storage.name"
            :aria-checked="storage.id === defaultStorage || draft.buckets.includes(storage.id)"
            :aria-disabled="storage.id === defaultStorage || !canEditPermissions || undefined"
            class="flex w-full items-center justify-between gap-3 border-b border-oi-line px-3.5 py-2.5 text-left text-[13px] last:border-b-0 hover:bg-oi-hover focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-inset focus-visible:ring-oi-ring aria-disabled:cursor-default aria-disabled:hover:bg-transparent"
            @click="toggleBucket(storage.id)"
          >
            <span class="flex min-w-0 items-center gap-2">
              <StorageTypeIcon v-if="storage.type" :type="storage.type" size="sm" />
              <span class="truncate">{{ storage.name }}</span>
              <span v-if="storage.id === defaultStorage" class="inline-flex h-[18px] flex-none items-center rounded-full bg-oi-panel px-1.5 text-[11px] text-oi-muted">默认</span>
              <span v-if="storage.disabled" class="inline-flex h-[18px] flex-none items-center rounded-full bg-oi-warn-soft px-1.5 text-[11px] text-oi-warn" title="授权保留，重新启用后即可上传">已停用</span>
            </span>
            <span
              :class="['grid h-[18px] w-[18px] flex-none place-items-center rounded-md border-[1.5px] text-[13px]', storage.id === defaultStorage || draft.buckets.includes(storage.id) ? 'border-oi-accent bg-oi-accent text-white' : 'border-oi-line-2 bg-oi-raised text-transparent', storage.id === defaultStorage && 'opacity-60']"
              aria-hidden="true"
            ><i class="ri-check-line" /></span>
          </button>
          <p v-if="!storageRows.length" class="px-3.5 py-4 text-center text-[13px] text-oi-muted">没有可以授权的存储</p>
        </div>
      </div>
    </form>

    <template #footer="{ close }">
      <UiButton variant="ghost" @click="close">取消</UiButton>
      <UiButton variant="primary" type="submit" form="user-drawer-form" :disabled="saving || (!canEditRole && !canEditPermissions)">
        <i v-if="saving" class="ri-loader-4-line animate-spin" aria-hidden="true" />保存
      </UiButton>
    </template>
  </UiDrawer>
</template>
