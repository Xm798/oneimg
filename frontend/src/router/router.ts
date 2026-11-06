import { api, ApiError, type User } from "@/api/client.ts"
import { createRouter, createWebHistory, type RouteComponent, type RouteLocationNormalized, type RouteMeta, type RouteRecordRaw } from 'vue-router'
import { canAccessRoute, setSessionUser, clearSession, type SessionUser } from '@/composables/useSession'
import { loadSiteConfig, useSite } from '@/composables/useSite'
import { toast } from '@/components/ui/toast'
import { getStoredUser } from '@/utils/permissions'
import { navItem } from './nav'

const site = useSite()

const navRoute = (name: string, component: () => Promise<RouteComponent>, meta: RouteMeta = {}): RouteRecordRaw => {
  const item = navItem(name)
  return { path: item.path, name, component, meta: { title: item.title, permissions: item.permissions, ...meta } }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'Login',
    component: () => import('@/views/Login.vue'),
    meta: { title: '登录', public: true, auth: true }
  },
  {
    path: '/register',
    name: 'Register',
    component: () => import('@/views/Register.vue'),
    meta: { title: '注册', public: true, auth: true }
  },
  navRoute('Upload', () => import('@/views/Upload.vue'), { bleed: true }),
  navRoute('Gallery', () => import('@/views/Gallery.vue'), { bleed: true }),
  navRoute('Stats', () => import('@/views/Stats.vue')),
  navRoute('Storage', () => import('@/views/Storage.vue')),
  navRoute('Users', () => import('@/views/Users.vue')),
  navRoute('Settings', () => import('@/views/Settings.vue')),
  navRoute('Account', () => import('@/views/Account.vue')),
  // 旧地址继续可用，避免书签失效
  { path: '/', redirect: '/upload' },
  { path: '/buckets', redirect: to => ({ path: '/storage', query: to.query, hash: to.hash }) },
  { path: '/tags', redirect: to => ({ path: '/gallery', query: { ...to.query, view: 'tags' } }) },
  { path: '/:pathMatch(.*)*', redirect: '/upload' },
]

// 开发环境专用：新界面基础组件预览，生产构建不包含这个页面
if (import.meta.env.DEV) {
  routes.push({
    path: '/_ui',
    name: 'UiShowcase',
    component: () => import('@/views/dev/UiShowcase.vue'),
    meta: { title: '组件预览', public: true }
  })
}

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes
})

const getPageTitle = (to: RouteLocationNormalized) => to.meta.title ? `${to.meta.title} - ${site.title}` : site.title

const loginRedirect = (to: RouteLocationNormalized) => to.fullPath === '/' ? '/login' : { path: '/login', query: { redirect: to.fullPath } }

const accessTarget = (to: RouteLocationNormalized, user: SessionUser) => {
  // 需要改密时只允许进入账户设置；无权访问的页面回到上传页
  if (!canAccessRoute(String(to.name), to.meta.permissions, user)) {
    return user.password_change_required ? '/account' : '/upload'
  }
  return true
}

const TRANSIENT_NOTICE_INTERVAL = 30_000
let lastTransientNotice = 0

const noticeTransientFailure = () => {
  const now = Date.now()
  if (now - lastTransientNotice < TRANSIENT_NOTICE_INTERVAL) return
  lastTransientNotice = now
  toast.error('暂时无法确认登录状态，请稍后重试')
}

router.beforeEach(async (to) => {
  await loadSiteConfig()
  document.title = getPageTitle(to)

  if (to.meta.public) return true

  const userInfo: SessionUser = getStoredUser()
  // 未登录时记下原地址，登录后回到这里
  if (!userInfo.username) {
    clearSession()
    return loginRedirect(to)
  }

  // 向服务器确认登录状态，并刷新角色、权限和改密要求
  let me: User
  try {
    me = await api.me()
  } catch (error) {
    if (error instanceof ApiError && [401, 403].includes(Number(error.problem?.status))) {
      // 会话失效：回到登录页，登录后返回原地址
      clearSession()
      return loginRedirect(to)
    }
    // 服务器暂时不可用或响应无法解析时沿用本地登录信息；接口本身仍会逐个鉴权，下次导航再确认
    noticeTransientFailure()
    return accessTarget(to, userInfo)
  }
  if (!me?.username || userInfo.username !== me.username) {
    clearSession()
    return '/login'
  }

  const current: SessionUser = {
    ...userInfo,
    id: me.id ?? userInfo.id,
    role: me.role,
    permission: me.permission || { codes: [], bucket_ids: [] },
    password_change_required: me.password_change_required,
    bootstrap_verification_required: me.bootstrap_verification_required,
  }
  setSessionUser(current)
  return accessTarget(to, current)
})

// 被取消的导航（如设置页「留在本页」）也会调用 afterEach，这时不改标题
router.afterEach((to, _from, failure) => {
  if (!failure) document.title = getPageTitle(to)
})

export default router
