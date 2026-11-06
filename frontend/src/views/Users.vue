<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage, isStaleError, type ManagedUser } from '@/api/client.ts'
import LinkBox from '@/components/media/LinkBox.vue'
import { confirm, toast, UiAvatar, UiButton, UiDialog, UiMenu, UiMenuItem, UiMenuSeparator, UiSegmented } from '@/components/ui'
import UserCreateDialog from '@/components/users/UserCreateDialog.vue'
import UserDrawer from '@/components/users/UserDrawer.vue'
import { ALL_CODES } from '@/components/users/permissions'
import type { UserStorage } from '@/components/users/types'
import { useSession } from '@/composables/useSession'
import { canManage, effectiveCodes, hasPermission, ROLE_ADMIN, SUPER_ADMIN_ID } from '@/utils/permissions.ts'

const PAGE_SIZE = 20
const route = useRoute()
const router = useRouter()
const { user: me } = useSession()
const can = (code: string) => hasPermission(code, me.value)
const canCreate = computed(() => can('user:create'))
const canEditRole = computed(() => can('user:role:update'))
const canEditPermissions = computed(() => can('user:permission:update'))
const canReset = computed(() => can('user:password:reset'))
const canRevokePasskeys = computed(() => can('user:passkey:reset'))
const canDelete = computed(() => can('user:delete'))
const hasMoreActions = computed(() => canReset.value || canRevokePasskeys.value || canDelete.value)

/* ---------- 筛选（同步到地址栏） ---------- */
const query = ref(typeof route.query.q === 'string' ? route.query.q : '')
const searchInput = ref(query.value)
const role = ref(['1', '3'].includes(String(route.query.role)) ? String(route.query.role) : 'all')
const page = ref(Math.max(1, Number(route.query.page) || 1))
const roleOptions = [
  { value: 'all', label: '全部' },
  { value: '1', label: '管理员' },
  { value: '3', label: '普通用户' },
]
let searchTimer: ReturnType<typeof setTimeout> | undefined
watch(searchInput, value => {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => { query.value = value.trim(); page.value = 1 }, 300)
})
watch(role, () => { page.value = 1 })
onBeforeUnmount(() => clearTimeout(searchTimer))

/* ---------- 列表 ---------- */
const users = ref<ManagedUser[]>([])
const total = ref(0)
const totalPages = ref(1)
const loading = ref(true)
const loadError = ref('')
let sequence = 0

const load = async () => {
  const current = ++sequence
  loading.value = true
  loadError.value = ''
  const params = new URLSearchParams({ page: String(page.value), page_size: String(PAGE_SIZE), sort: 'id', order: 'asc' })
  if (query.value) params.set('username', query.value)
  if (role.value !== 'all') params.set('role', role.value)
  try {
    const result = await api.listUsersPage(params.toString())
    if (current !== sequence) return
    users.value = result.data ?? []
    total.value = result.meta?.pagination?.total ?? users.value.length
    totalPages.value = Math.max(1, result.meta?.pagination?.total_pages ?? 1)
    // 删除最后一页的最后一个用户后回到上一页
    if (page.value > totalPages.value) page.value = totalPages.value
  } catch (error) {
    if (current !== sequence) return
    loadError.value = errorMessage(error, '读取用户列表失败')
  } finally {
    if (current === sequence) loading.value = false
  }
}

watch([query, role, page], () => {
  const next: Record<string, string> = {}
  if (query.value) next.q = query.value
  if (role.value !== 'all') next.role = role.value
  if (page.value > 1) next.page = String(page.value)
  void router.replace({ query: next })
  void load()
})

/* ---------- 存储（权限抽屉用） ---------- */
const storages = ref<UserStorage[]>([])
const storagesPartial = ref(false)
const defaultStorage = ref(0)
const loadStorages = async () => {
  try {
    const options = await api.uploadOptions()
    defaultStorage.value = Number(options.default_storage) || 0
    try {
      storages.value = (await api.listBuckets()).map(item => ({ id: item.id, name: item.name, type: item.type, disabled: item.disabled }))
    } catch {
      // 没有存储管理权限时读不到完整列表，退回到当前可上传的存储
      const list = Array.isArray(options.storage_buckets) ? options.storage_buckets as Record<string, unknown>[] : []
      storages.value = list.map(item => ({ id: Number(item.id), name: String(item.name ?? ''), type: String(item.type ?? ''), disabled: false }))
      storagesPartial.value = true
    }
  } catch {
    storagesPartial.value = true
  }
}

onMounted(() => {
  void load()
  if (canEditPermissions.value) void loadStorages()
})

/* ---------- 显示 ---------- */
const manageable = (user: ManagedUser) => canManage(user, me.value)
const reloadOnStale = (error: unknown) => {
  if (isStaleError(error)) void load()
}
const codeCount = (user: ManagedUser) => {
  if (user.role !== ROLE_ADMIN) return '—'
  if (user.id === SUPER_ADMIN_ID || (user.permission?.codes ?? []).includes('*')) return '全部'
  return `${effectiveCodes(user).length} / ${ALL_CODES.length}`
}
const storageName = (id: number) => {
  const storage = storages.value.find(item => item.id === id)
  if (!storage) return `#${id}`
  return storage.disabled ? `${storage.name}（已停用）` : storage.name
}
const storageText = (user: ManagedUser) => {
  if (user.role === ROLE_ADMIN) return '全部存储'
  const granted = (user.permission?.bucket_ids ?? []).filter(id => id !== defaultStorage.value)
  if (!storages.value.length) return granted.length ? `默认存储 + ${granted.length} 个` : '默认存储'
  return ['默认存储', ...granted.map(storageName)].join('、')
}
const formatDate = (value: string) => {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleDateString('zh-CN')
}

/* ---------- 新建、编辑 ---------- */
const createOpen = ref(false)
const drawerOpen = ref(false)
const editing = ref<ManagedUser | null>(null)
const openDrawer = (user: ManagedUser) => {
  editing.value = user
  drawerOpen.value = true
}

/* ---------- 重置密码、撤销 Passkey、删除 ---------- */
const tempPassword = ref<{ username: string; password: string } | null>(null)
const tempOpen = ref(false)
const resetPassword = async (user: ManagedUser) => {
  const ok = await confirm({
    title: `重置 ${user.username} 的密码？`,
    description: '将生成 16 位临时密码：',
    points: ['旧密码立即失效，该用户已登录的会话会退出', '该用户的全部 API 令牌和 Passkey 会被撤销', '该用户首次登录必须修改密码'],
    icon: 'ri-key-2-line',
    confirmText: '重置密码',
  })
  if (!ok) return
  try {
    const result = await api.resetUserPassword(user.id)
    tempPassword.value = { username: user.username, password: result.new_password }
    tempOpen.value = true
  } catch (error) {
    toast.error(errorMessage(error, '重置密码失败'))
    reloadOnStale(error)
  }
}
watch(tempOpen, value => { if (!value) tempPassword.value = null })

const revokePasskeys = async (user: ManagedUser) => {
  const ok = await confirm({
    title: `撤销 ${user.username} 的全部 Passkey？`,
    description: `共 ${user.passkey_count} 个。撤销后这些设备不能再用于登录，用户需要用密码登录后重新添加。`,
    danger: true,
    icon: 'ri-fingerprint-line',
    confirmText: '全部撤销',
  })
  if (!ok) return
  try {
    await api.revokeUserPasskeys(user.id)
    toast.success(`已撤销 ${user.username} 的 Passkey`)
    void load()
  } catch (error) {
    toast.error(errorMessage(error, '撤销失败'))
    reloadOnStale(error)
  }
}

const removeUser = async (user: ManagedUser) => {
  const ok = await confirm({
    title: `删除用户 ${user.username}？`,
    description: '删除后无法恢复：',
    points: ['账号及其 Passkey、API 令牌会被删除', '已上传的文件会保留，上传者显示为「已删除用户」'],
    danger: true,
    confirmText: '删除用户',
  })
  if (!ok) return
  try {
    await api.deleteUser(user.id)
    toast.success(`已删除用户 ${user.username}`)
    void load()
  } catch (error) {
    toast.error(errorMessage(error, '删除失败'))
    reloadOnStale(error)
  }
}
</script>

<template>
  <div class="mx-auto w-full max-w-[1240px] font-oi text-sm text-oi-text">
    <header class="mb-[22px] flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 class="text-2xl font-[680] leading-tight tracking-[-0.02em] text-oi-ink">用户</h1>
        <p class="mt-1.5 text-oi-muted">管理账号、角色、功能权限与可用存储。</p>
      </div>
      <UiButton v-if="canCreate" variant="primary" @click="createOpen = true"><i class="ri-user-add-line" aria-hidden="true" />新建用户</UiButton>
    </header>

    <div class="mb-3.5 flex flex-wrap items-center gap-2.5">
      <label class="relative w-[240px] max-[560px]:w-full">
        <span class="sr-only">搜索用户名</span>
        <i class="ri-search-line pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-oi-muted" aria-hidden="true" />
        <input
          v-model="searchInput"
          type="search"
          placeholder="搜索用户名"
          class="h-9 w-full rounded-[10px] border border-oi-line-2 bg-oi-raised pl-9 pr-3 text-[13.5px] text-oi-ink outline-none placeholder:text-oi-faint focus:border-oi-accent focus:shadow-[0_0_0_3px_var(--oi-ring)]"
        >
      </label>
      <UiSegmented v-model="role" :options="roleOptions" aria-label="按角色筛选" />
    </div>

    <div v-if="loadError" class="mb-4 flex flex-wrap items-center gap-3 rounded-oi border border-oi-line bg-oi-danger-soft px-4 py-3 text-[13px] text-oi-danger" role="alert">
      <i class="ri-error-warning-line" aria-hidden="true" />
      <span class="min-w-0 flex-1">{{ loadError }}</span>
      <UiButton size="sm" @click="load">重试</UiButton>
    </div>

    <section class="-mx-4 overflow-x-auto px-4 min-[900px]:mx-0 min-[900px]:overflow-visible min-[900px]:px-0" :aria-busy="loading">
      <table :class="['w-full border-separate border-spacing-0 transition-opacity', loading && users.length && 'opacity-60']">
        <thead>
          <tr class="text-left text-xs font-[550] text-oi-muted [&>th]:whitespace-nowrap [&>th]:border-b [&>th]:border-oi-line [&>th]:px-3 [&>th]:py-2 [&>th]:font-[550]">
            <th scope="col">用户</th>
            <th scope="col">角色</th>
            <th scope="col" class="max-[900px]:hidden">功能权限</th>
            <th scope="col" class="max-[900px]:hidden">可用存储</th>
            <th scope="col" class="max-[900px]:hidden">Passkey</th>
            <th scope="col" class="max-[900px]:hidden">创建</th>
            <th scope="col"><span class="sr-only">操作</span></th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="user in users"
            :key="user.id"
            class="hover:[&>td]:bg-oi-panel [&>td]:border-b [&>td]:border-oi-line [&>td]:px-3 [&>td]:py-[9px] [&>td]:align-middle"
          >
            <td>
              <div class="flex min-w-0 items-center gap-2.5">
                <UiAvatar :name="user.username" :src="user.avatar_url" />
                <div class="min-w-0">
                  <b class="block max-w-[220px] truncate font-[580] text-oi-ink" :title="user.username">{{ user.username }}</b>
                  <div class="text-xs text-oi-muted">ID {{ user.id }}<template v-if="user.id === me.id"> · 你</template></div>
                </div>
              </div>
            </td>
            <td class="whitespace-nowrap">
              <span v-if="user.id === SUPER_ADMIN_ID" class="inline-flex h-[22px] items-center gap-1 rounded-full bg-oi-sakura-soft px-2 text-xs font-[550] text-oi-sakura"><i class="ri-vip-crown-2-line" aria-hidden="true" />超级管理员</span>
              <span
                v-else
                :class="['inline-flex h-[22px] items-center rounded-full px-2 text-xs font-[550]', user.role === ROLE_ADMIN ? 'bg-oi-accent-soft text-oi-accent-2' : 'bg-oi-panel text-oi-text']"
              >{{ user.role === ROLE_ADMIN ? '管理员' : '普通用户' }}</span>
            </td>
            <td class="whitespace-nowrap tabular-nums text-oi-muted max-[900px]:hidden">{{ codeCount(user) }}</td>
            <td class="max-w-[240px] text-oi-muted max-[900px]:hidden">{{ storageText(user) }}</td>
            <td class="tabular-nums text-oi-muted max-[900px]:hidden">{{ user.passkey_count || '—' }}</td>
            <td class="whitespace-nowrap tabular-nums text-oi-muted max-[900px]:hidden">{{ formatDate(user.created_at) }}</td>
            <td class="whitespace-nowrap text-right">
              <template v-if="manageable(user)">
                <UiButton v-if="canEditRole || canEditPermissions" size="sm" @click="openDrawer(user)">权限</UiButton>
                <UiMenu v-if="hasMoreActions" align="end">
                  <template #trigger>
                    <UiButton size="sm" variant="ghost" icon class="ml-1" :aria-label="`${user.username} 的更多操作`"><i class="ri-more-fill" aria-hidden="true" /></UiButton>
                  </template>
                  <UiMenuItem v-if="canReset" icon="ri-key-2-line" @select="resetPassword(user)">重置密码</UiMenuItem>
                  <UiMenuItem v-if="canRevokePasskeys" icon="ri-fingerprint-line" :disabled="!user.passkey_count" @select="revokePasskeys(user)">
                    撤销全部 Passkey<template v-if="!user.passkey_count" #end><span class="text-xs text-oi-faint">无</span></template>
                  </UiMenuItem>
                  <template v-if="canDelete">
                    <UiMenuSeparator v-if="canReset || canRevokePasskeys" />
                    <UiMenuItem icon="ri-delete-bin-6-line" danger @select="removeUser(user)">删除用户</UiMenuItem>
                  </template>
                </UiMenu>
              </template>
              <RouterLink v-else-if="user.id === me.id" to="/account" class="text-xs text-oi-muted hover:text-oi-ink hover:underline">在账户设置中修改</RouterLink>
              <span v-else-if="user.id === SUPER_ADMIN_ID" class="text-xs text-oi-muted">受保护</span>
              <span v-else class="text-xs text-oi-muted" title="只能管理权限少于自己的管理员和普通用户">无权管理</span>
            </td>
          </tr>
          <tr v-if="loading && !users.length">
            <td colspan="7" class="px-3 py-10 text-center text-oi-muted"><i class="ri-loader-4-line animate-spin" aria-hidden="true" /> 正在读取</td>
          </tr>
          <tr v-else-if="!loading && !users.length && !loadError">
            <td colspan="7" class="px-3 py-12 text-center text-oi-muted">{{ query || role !== 'all' ? '没有符合条件的用户' : '还没有用户' }}</td>
          </tr>
        </tbody>
      </table>
    </section>

    <nav v-if="total > 0" class="flex items-center justify-center gap-3 pb-1 pt-6 text-[12.5px] text-oi-muted" aria-label="分页">
      <UiButton size="sm" variant="ghost" :disabled="page <= 1 || loading" @click="page--"><i class="ri-arrow-left-s-line" aria-hidden="true" />上一页</UiButton>
      <span aria-live="polite">第 {{ page }} / {{ totalPages }} 页 · 共 {{ total }} 人</span>
      <UiButton size="sm" variant="ghost" :disabled="page >= totalPages || loading" @click="page++">下一页<i class="ri-arrow-right-s-line" aria-hidden="true" /></UiButton>
    </nav>

    <UserCreateDialog v-model:open="createOpen" :can-create-admin="canEditRole" @created="load" />
    <UserDrawer
      v-model:open="drawerOpen"
      :user="editing"
      :storages="storages"
      :default-storage="defaultStorage"
      :storages-partial="storagesPartial"
      :can-edit-role="canEditRole"
      :can-edit-permissions="canEditPermissions"
      :actor="me"
      @saved="load"
    />
    <UiDialog v-model:open="tempOpen" :title="`${tempPassword?.username ?? ''} 的临时密码`" description="只显示这一次，请通过安全渠道发给对方，并提醒对方登录后在账户设置里修改密码。" icon="ri-key-2-line">
      <LinkBox v-if="tempPassword" :value="tempPassword.password" label="临时密码" />
      <template #footer="{ close }">
        <UiButton variant="primary" @click="close">完成</UiButton>
      </template>
    </UiDialog>
  </div>
</template>
