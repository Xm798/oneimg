<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { api, ApiError, errorMessage, type User } from '@/api/client.ts'
import { passwordError, usernameError } from '@/components/auth/auth'
import { toast, UiButton, UiDialog, UiField, UiInput, UiPasswordInput, UiSegmented } from '@/components/ui'
import { ROLE_ADMIN, ROLE_USER } from '@/utils/permissions.ts'

/** Creating an admin requires user:role:update. */
defineProps<{ canCreateAdmin: boolean }>()
const open = defineModel<boolean>('open', { default: false })
const emit = defineEmits<{ created: [user: User] }>()

const form = reactive({ username: '', password: '', role: String(ROLE_USER) })
const errors = reactive<Record<string, string>>({})
const formError = ref('')
const saving = ref(false)
const roleOptions = [
  { value: String(ROLE_USER), label: '普通用户' },
  { value: String(ROLE_ADMIN), label: '管理员' },
]

watch(open, value => {
  if (!value) return
  Object.assign(form, { username: '', password: '', role: String(ROLE_USER) })
  for (const key of Object.keys(errors)) delete errors[key]
  formError.value = ''
})

const submit = async () => {
  if (saving.value) return
  for (const key of Object.keys(errors)) delete errors[key]
  formError.value = ''
  const name = usernameError(form.username)
  const pwd = passwordError(form.password)
  if (name) errors.username = name
  if (pwd) errors.password = pwd
  if (Object.keys(errors).length) return
  saving.value = true
  try {
    const user = await api.createUser({ username: form.username.trim(), password: form.password, role: Number(form.role) })
    toast.success(`已创建用户 ${user.username}`)
    open.value = false
    emit('created', user)
  } catch (error) {
    const code = error instanceof ApiError ? error.problem?.code : ''
    const message = errorMessage(error, '创建失败，请稍后重试')
    if (code === 'username_conflict' || message.includes('用户名')) errors.username = message
    else if (message.includes('密码')) errors.password = message
    else formError.value = message
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UiDialog v-model:open="open" title="新建用户" description="创建后可以在「权限」中设置可用存储或功能权限。" icon="ri-user-add-line">
    <form id="user-create-form" class="flex flex-col gap-3.5" novalidate @submit.prevent="submit">
      <UiField label="用户名" for="user-create-name" hint="3–50 个字符" :error="errors.username">
        <UiInput id="user-create-name" v-model="form.username" maxlength="50" autocomplete="off" :invalid="!!errors.username" />
      </UiField>
      <UiField label="初始密码" for="user-create-password" hint="至少 6 位。新用户首次登录后可以在账户设置里修改" :error="errors.password">
        <UiPasswordInput id="user-create-password" v-model="form.password" maxlength="100" autocomplete="new-password" :invalid="!!errors.password" />
      </UiField>
      <UiField v-if="canCreateAdmin" label="角色">
        <UiSegmented v-model="form.role" :options="roleOptions" aria-label="角色" class="self-start" />
        <template #hint>{{ form.role === String(ROLE_ADMIN) ? '管理员可以使用全部存储，后台功能按勾选的权限开放（新建时没有任何功能权限）。' : '普通用户只能管理自己的文件，可以使用默认存储和授权的存储。' }}</template>
      </UiField>
      <p v-if="formError" class="flex items-center gap-1.5 text-[12.5px] text-oi-danger" role="alert"><i class="ri-error-warning-line" aria-hidden="true" />{{ formError }}</p>
    </form>
    <template #footer="{ close }">
      <UiButton variant="ghost" @click="close">取消</UiButton>
      <UiButton variant="primary" type="submit" form="user-create-form" :disabled="saving">
        <i v-if="saving" class="ri-loader-4-line animate-spin" aria-hidden="true" />创建
      </UiButton>
    </template>
  </UiDialog>
</template>
